/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/xanzy/go-cloudstack/cloudstack"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog"
)

const (
	lbNameLabel     = "csccm.cloudprovider.io/loadbalancer-name"
	lbNameSuffix    = "csccm.cloudprovider.io/loadbalancer-name-suffix"
	lbUseTargetPort = "csccm.cloudprovider.io/loadbalancer-use-targetport"

	associateIPAddressExtraParamPrefix = "csccm.cloudprovider.io/associateipaddress-extra-param-"
	createLoadBalancerExtraParamPrefix = "csccm.cloudprovider.io/createloadbalancer-extra-param-"
	lbCustomHealthCheck                = "csccm.cloudprovider.io/loadbalancer-custom-healthcheck"
	lbCustomHealthCheckMessagePrefix   = "csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-"
	lbCustomHealthCheckResponsePrefix  = "csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-"

	cloudProviderTag = "cloudprovider"
	serviceTag       = "kubernetes_service"
	namespaceTag     = "kubernetes_namespace"

	CloudstackResourceIPAdress     = "PublicIpAddress"
	CloudstackResourceLoadBalancer = "LoadBalancer"
)

type projectCloud struct {
	*CSCloud
	projectID   string
	environment string
}

type loadBalancer struct {
	cloud *projectCloud

	name          string
	algorithm     string
	mainNetworkID string
	ip            cloudstackIP
	rule          *loadBalancerRule
}

type cloudstackIP struct {
	id      string
	address string
}

type loadBalancerRule struct {
	*cloudstack.LoadBalancerRule
	AdditionalPortMap []string `json:"additionalportmap"`
}

type globoNetworkPools struct {
	Count             int                 `json:"count"`
	GloboNetworkPools []*globoNetworkPool `json:"globonetworkpool"`
}

type globoNetworkPool struct {
	Port                int    `json:"port"`
	VipPort             int    `json:"vipport"`
	HealthCheckType     string `json:"healthchecktype"`
	HealthCheck         string `json:"healthcheck"`
	HealthCheckExpected string `json:"healthcheckexpected"`
	Id                  int    `json:"id"`
}

type UpdateGloboNetworkPoolResponse struct {
	JobID     string `json:"jobid"`
	Jobstatus int    `json:"jobstatus"`
}

func (ip cloudstackIP) String() string {
	return fmt.Sprintf("ip(%v, %v)", ip.id, ip.address)
}

func (ip cloudstackIP) isValid() bool {
	return ip.id != "" && ip.address != ""
}

// GetLoadBalancer returns whether the specified load balancer exists, and if so, what its status is.
func (cs *CSCloud) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	klog.V(5).Infof("GetLoadBalancer(%v, %v, %v)", clusterName, service.Namespace, service.Name)
	cs.svcLock.Lock(service)
	defer cs.svcLock.Unlock(service)

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, "", nil)
	if err != nil {
		return nil, false, err
	}

	// If we don't have a rule, the load balancer does not exist.
	if lb.rule == nil {
		return nil, false, nil
	}

	klog.V(4).Infof("Found a load balancer associated with IP %v", lb.ip)

	status := &v1.LoadBalancerStatus{}
	status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: lb.ip.address})

	return status, true, nil
}

// EnsureLoadBalancer creates a new load balancer, or updates the existing one. Returns the status of the balancer.
func (cs *CSCloud) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (status *v1.LoadBalancerStatus, err error) {
	klog.V(5).Infof("EnsureLoadBalancer(%v, %v, %v, %v, %v, %#v)", clusterName, service.Namespace, service.Name, service.Spec.LoadBalancerIP, service.Spec.Ports, nodes)
	cs.svcLock.Lock(service)
	defer cs.svcLock.Unlock(service)

	if len(service.Spec.Ports) == 0 {
		return nil, fmt.Errorf("requested load balancer with no ports")
	}

	nodes = cs.filterNodesMatchingLabels(nodes, *service)

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes available to add to load balancer")
	}

	hostIDs, networkIDs, projectID, err := cs.extractIDs(nodes)
	if err != nil {
		return nil, err
	}
	if len(hostIDs) == 0 || len(networkIDs) == 0 {
		return nil, fmt.Errorf("unable to map kubernetes nodes to cloudstack instances and networks for service (%v, %v)", service.Namespace, service.Name)
	}

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, projectID, networkIDs)
	if err != nil {
		return nil, err
	}

	lb.setAlgorithm(service)

	klog.V(4).Infof("Ensuring Load Balancer: %#v", lb)

	if lb.rule != nil {
		var manage bool
		manage, err = shouldManageLB(lb, service)
		if err != nil {
			return nil, fmt.Errorf("failed to check if LB should be managed: %v", err)
		}
		if !manage {
			klog.V(3).Infof("Skipping EnsureLoadBalancer for service %s/%s and LB %s", service.Namespace, service.Name, lb.ip)
			return nil, fmt.Errorf("LB %s not managed by cloudprovider", lb.ip)
		}
	}

	err = lb.loadLoadBalancerIP(service)
	if err != nil {
		return nil, err
	}

	if service.Spec.LoadBalancerIP != "" && lb.ip.address != service.Spec.LoadBalancerIP {
		err = lb.updateLoadBalancerIP(service)
		if err != nil {
			return nil, err
		}
	}

	klog.V(4).Infof("Load balancer %v is associated with IP %v", lb.name, lb.ip)

	status = &v1.LoadBalancerStatus{}
	status.Ingress = []v1.LoadBalancerIngress{{IP: lb.ip.address, Hostname: lb.name}}

	if lb.cloud.projectID != "" && service.Labels[cs.config.Global.ProjectIDLabel] == "" {
		service.Labels[cs.config.Global.ProjectIDLabel] = lb.cloud.projectID
	}

	// If the load balancer rule exists and is up-to-date, we move on to the next rule.
	exists, needsUpdate, err := lb.checkLoadBalancerRule(lb.name, service)
	if err != nil {
		return nil, err
	}

	if needsUpdate {
		klog.V(4).Infof("Updating load balancer rule: %v", lb.name)
		if err = lb.updateLoadBalancerRule(service); err != nil {
			return nil, err
		}
	}

	if exists {
		if err = lb.syncNodes(hostIDs, networkIDs); err != nil {
			return nil, err
		}
		if err = lb.updateLoadBalancerPool(lb.rule, service); err != nil {
			return nil, err
		}
		return status, nil
	}

	klog.V(4).Infof("Creating load balancer rule: %v", lb.name)
	lb.rule, err = lb.createLoadBalancerRule(lb.name, service)
	if err != nil {
		return nil, err
	}

	klog.V(4).Infof("Assigning tag to load balancer rule: %v", lb.name)
	if err = lb.cloud.assignTagsToRule(lb.rule, service); err != nil {
		return nil, err
	}

	if err = lb.syncNodes(hostIDs, networkIDs); err != nil {
		return nil, err
	}

	if err = lb.updateLoadBalancerPool(lb.rule, service); err != nil {
		return nil, err
	}

	return status, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (cs *CSCloud) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	klog.V(5).Infof("UpdateLoadBalancer(%v, %v, %v, %#v)", clusterName, service.Namespace, service.Name, nodes)
	cs.svcLock.Lock(service)
	defer cs.svcLock.Unlock(service)

	nodes = cs.filterNodesMatchingLabels(nodes, *service)

	hostIDs, networkIDs, projectID, err := cs.extractIDs(nodes)
	if err != nil {
		return err
	}

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, projectID, networkIDs)
	if err != nil {
		return err
	}

	if lb.rule == nil {
		return nil
	}

	manage, err := shouldManageLB(lb, service)
	if err != nil {
		return fmt.Errorf("failed to check if LB should be managed: %v", err)
	}
	if !manage {
		klog.V(3).Infof("Skipping UpdateLoadBalancer for service %s/%s and LB %s", service.Namespace, service.Name, lb.ip)
		return nil
	}

	return lb.syncNodes(hostIDs, networkIDs)
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it exists, returning
// nil if the load balancer specified either didn't exist or was successfully deleted.
func (cs *CSCloud) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	klog.V(5).Infof("EnsureLoadBalancerDeleted(%v, %v, %v)", clusterName, service.Namespace, service.Name)
	cs.svcLock.Lock(service)
	defer cs.svcLock.Unlock(service)

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, "", nil)
	if err != nil {
		return err
	}

	if !lb.cloud.environments[lb.cloud.environment].removeLBs {
		klog.V(3).Infof("skipping deletion of load balancer %s: environment has removals disabled.", lb.ip)
		return nil
	}

	manage, err := shouldManageLB(lb, service)
	if err != nil {
		return fmt.Errorf("failed to check if LB should be managed: %v", err)
	}
	if !manage {
		klog.V(3).Infof("Skipping EnsureLoadBalancerDeleted for service %s/%s and LB %s", service.Namespace, service.Name, lb.ip)
		return nil
	}

	klog.V(4).Infof("Deleting load balancer provider tag: %v", lb.ip)
	if err := lb.deleteTags(service); err != nil {
		return err
	}

	if lb.rule != nil {
		if err := lb.deleteLoadBalancerRule(); err != nil {
			return err
		}
	}

	if lb.ip.id != "" && lb.ip.address != service.Spec.LoadBalancerIP {
		klog.V(4).Infof("Releasing load balancer IP: %v", lb.ip)
		if err := lb.cloud.releaseIPIfManaged(lb.ip, service); err != nil {
			return err
		}
	}

	return nil
}

// getLoadBalancer retrieves the IP address and ID and all the existing rules it can find.
func (cs *CSCloud) getLoadBalancer(service *v1.Service, projectID string, networkIDs []string) (*loadBalancer, error) {
	if projectID == "" {
		var ok bool
		projectID, ok = getLabelOrAnnotation(service.ObjectMeta, cs.config.Global.ProjectIDLabel)
		if !ok {
			klog.V(4).Infof("unable to retrieve projectID for service: %#v", service)
		}
	}
	environment, _ := getLabelOrAnnotation(service.ObjectMeta, cs.config.Global.EnvironmentLabel)
	lb := &loadBalancer{
		cloud: &projectCloud{
			CSCloud:     cs,
			environment: environment,
			projectID:   projectID,
		},
		name: cs.getLoadBalancerName(service),
	}
	if len(networkIDs) > 0 {
		lb.mainNetworkID = networkIDs[0]
	}

	client, err := lb.getClient()
	if err != nil {
		klog.V(4).Infof("unable to retrieve cloudstack client for load balancer %q for service %v/%v: %v", lb.name, service.Namespace, service.Name, err)
		return lb, nil
	}

	lb.rule, err = getLoadBalancerRule(client, service, lb.name, projectID)
	if err != nil {
		return nil, fmt.Errorf("load balancer %s for service %v/%v get rule error: %v", lb.name, service.Namespace, service.Name, err)
	}

	if lb.rule != nil {
		lb.ip = cloudstackIP{
			address: lb.rule.Publicip,
			id:      lb.rule.Publicipid,
		}
		lb.mainNetworkID = lb.rule.Networkid
	}

	return lb, nil
}

func getLoadBalancerRule(client *cloudstack.CloudStackClient, service *v1.Service, lbName, projectID string) (*loadBalancerRule, error) {
	lb, err := getLoadBalancerRuleByName(client, lbName, projectID)
	if lb == nil && err == nil {
		lb, err = getLoadBalancerByTags(client, service, projectID)
	}
	if err != nil {
		return nil, err
	}
	return lb, nil
}

func getLoadBalancerRuleByName(client *cloudstack.CloudStackClient, lbName, projectID string) (*loadBalancerRule, error) {
	pc := &cloudstack.CustomServiceParams{}

	pc.SetParam("keyword", lbName)
	pc.SetParam("listall", true)
	if projectID != "" {
		pc.SetParam("projectid", projectID)
	}

	var result struct {
		Count             int                 `json:"count"`
		LoadBalancerRules []*loadBalancerRule `json:"loadbalancerrule"`
	}

	err := client.Custom.CustomRequest("listLoadBalancerRules", pc, &result)
	if err != nil {
		return nil, err
	}
	if result.Count == 0 {
		return nil, nil
	}

	var count int
	var lbResult *loadBalancerRule

	for _, lbRule := range result.LoadBalancerRules {
		if lbRule.Name == lbName {
			lbResult = lbRule
			count++
		}
	}
	if count > 1 {
		return nil, fmt.Errorf("lb %q too many rules associated: %#v", lbName, result.LoadBalancerRules)
	}
	if count == 0 {
		return nil, nil
	}
	return lbResult, nil
}

func getLoadBalancerByTags(client *cloudstack.CloudStackClient, service *v1.Service, projectID string) (*loadBalancerRule, error) {
	pc := &cloudstack.CustomServiceParams{}

	pc.SetParam("listall", true)
	if projectID != "" {
		pc.SetParam("projectid", projectID)
	}
	tags := tagsForService(service)
	// Use only service name in query, as we'll filter the result by all tags a
	// few lines down.
	pc.SetParam("tags[0].key", serviceTag)
	pc.SetParam("tags[0].value", tags[serviceTag])

	var result struct {
		Count             int                 `json:"count"`
		LoadBalancerRules []*loadBalancerRule `json:"loadbalancerrule"`
	}

	err := client.Custom.CustomRequest("listLoadBalancerRules", pc, &result)
	if err != nil {
		return nil, err
	}
	if result.Count == 0 {
		return nil, nil
	}

	var count int
	var lbResult *loadBalancerRule
	for _, lbRule := range result.LoadBalancerRules {
		if matchAllTags(lbRule.Tags, tags) {
			lbResult = lbRule
			count++
		}
	}
	if count > 1 {
		return nil, fmt.Errorf("tags %#v with too many rules associated: %#v", tags, result.LoadBalancerRules)
	}
	if count == 0 {
		return nil, nil
	}
	return lbResult, nil
}

// extractIDs extracts the VM ID for each node, their unique network IDs and project ID
func (cs *CSCloud) extractIDs(nodes []*v1.Node) ([]string, []string, string, error) {
	if len(nodes) == 0 {
		klog.V(4).Info("skipping extractIDs for empty node slice")
		return nil, nil, "", nil
	}

	environmentID, _ := getLabelOrAnnotation(nodes[0].ObjectMeta, cs.config.Global.EnvironmentLabel)

	projectID, ok := getLabelOrAnnotation(nodes[0].ObjectMeta, cs.config.Global.ProjectIDLabel)
	if !ok {
		return nil, nil, "", fmt.Errorf("unable to retrieve projectID for node %#v", nodes[0])
	}

	var manager *cloudstackManager
	if env, ok := cs.environments[environmentID]; ok {
		manager = env.manager
	}
	if manager == nil {
		return nil, nil, "", fmt.Errorf("unable to retrieve cloudstack manager for environment %q", environmentID)
	}

	var hostIDs []string
	var networkIDs []string
	networkMap := make(map[string]struct{})

	for _, node := range nodes {
		hostName := node.Name
		if name, ok := getLabelOrAnnotation(node.ObjectMeta, cs.config.Global.NodeNameLabel); ok {
			hostName = name
		}

		vm, err := manager.virtualMachineByName(hostName, projectID)
		if err != nil && err != ErrVMNotFound {
			return nil, nil, "", err
		}

		if vm != nil {
			hostIDs = append(hostIDs, vm.Id)
			nic, err := cs.externalNIC(vm)
			if err != nil {
				return nil, nil, "", err
			}
			if _, ok := networkMap[nic.Networkid]; ok {
				continue
			}
			networkMap[nic.Networkid] = struct{}{}
			networkIDs = append(networkIDs, nic.Networkid)
		}
	}

	return hostIDs, networkIDs, projectID, nil
}

func (cs *CSCloud) externalNIC(instance *cloudstack.VirtualMachine) (*cloudstack.Nic, error) {
	if len(instance.Nic) == 0 {
		return nil, errors.New("instance does not have any nics")
	}

	externalIndex := cs.config.Global.ExternalIPIndex
	if externalIndex >= 0 && externalIndex < len(instance.Nic) {
		return &instance.Nic[externalIndex], nil
	}
	return &instance.Nic[0], nil
}

func (cs *CSCloud) filterNodesMatchingLabels(nodes []*v1.Node, service v1.Service) []*v1.Node {
	if cs.config.Global.ServiceFilterLabel == "" || cs.config.Global.NodeFilterLabel == "" {
		return nodes
	}
	labelValue, _ := getLabelOrAnnotation(service.ObjectMeta, cs.config.Global.ServiceFilterLabel)

	var filteredNodes []*v1.Node
	for i := range nodes {
		nodeLabelValue, _ := getLabelOrAnnotation(nodes[i].ObjectMeta, cs.config.Global.NodeFilterLabel)
		if nodeLabelValue != labelValue {
			continue
		}
		filteredNodes = append(filteredNodes, nodes[i])
	}
	return filteredNodes
}

// GetLoadBalancerName returns the name of the load balancer responsible for
// the service by looking at the service label. If not set, it fallsback to the
// concatanation of the service name and the environment load balancer domain
// for the environent
func (cs *CSCloud) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return cs.getLoadBalancerName(service)
}

func (cs *CSCloud) getLoadBalancerName(service *v1.Service) string {
	name, _ := getLabelOrAnnotation(service.ObjectMeta, lbNameLabel)
	if name != "" {
		return name
	}
	suffix, _ := getLabelOrAnnotation(service.ObjectMeta, lbNameSuffix)
	if suffix != "" {
		return fmt.Sprintf("%s.%s", service.Name, suffix)
	}
	environment, _ := getLabelOrAnnotation(service.ObjectMeta, cs.config.Global.EnvironmentLabel)
	var lbDomain string
	if env, ok := cs.environments[environment]; ok {
		lbDomain = env.lbDomain
	}
	return fmt.Sprintf("%s.%s", service.Name, lbDomain)
}

func (lb *loadBalancer) loadLoadBalancerIP(service *v1.Service) error {
	if lb.ip.isValid() {
		return nil
	}
	ip, err := lb.cloud.getLoadBalancerIP(service, lb.mainNetworkID)
	if err != nil {
		return err
	}
	lb.ip = *ip
	return nil
}

func (lb *loadBalancer) updateLoadBalancerIP(service *v1.Service) error {
	publicIP, err := lb.cloud.getPublicIPAddressByIP(service.Spec.LoadBalancerIP)
	if err != nil {
		return err
	}
	if lb.rule != nil {
		err = lb.deleteLoadBalancerRule()
		if err != nil {
			return err
		}
		lb.rule = nil
	}
	if !lb.ip.isValid() {
		return nil
	}

	err = lb.cloud.releaseIPIfManaged(lb.ip, service)
	if err != nil {
		return err
	}
	lb.ip = *publicIP
	return nil
}

func (pc *projectCloud) releaseIPIfManaged(ip cloudstackIP, service *v1.Service) error {
	publicIP, err := pc.getPublicIPAddressByID(ip.id)
	if err != nil {
		return err
	}
	if shouldManageIP(*publicIP, service) {
		return pc.releaseLoadBalancerIP(ip)
	}
	return nil
}

// getLoadBalancerIP retrieves an existing IP for the loadbalancer or allocates
// a new one.
//
// This function should try to find an IP address with the following priorities:
// 1 - Find an existing public IP tagged for the service
// 2 - Find a public IP matching service's Spec.LoadBalancerIP
// 3 - Allocate a new random IP
//
// On situation 3 we'll also tag the IP address so that we can reuse or free it
// in the future. If tagging fails we should immediately release it.
func (pc *projectCloud) getLoadBalancerIP(service *v1.Service, networkID string) (*cloudstackIP, error) {
	klog.V(4).Infof("getLoadBalancerIP for service (%v, %v)", service.Namespace, service.Name)
	ip, err := pc.tryPublicIPAddressByTags(service)
	if err != nil {
		return nil, err
	}
	if ip != nil {
		return ip, nil
	}
	if service.Spec.LoadBalancerIP != "" {
		return pc.getPublicIPAddressByIP(service.Spec.LoadBalancerIP)
	}
	ip, err = pc.associatePublicIPAddress(service, networkID)
	if err != nil {
		return nil, err
	}
	err = pc.assignTagsToIP(ip, service)
	if err != nil {
		rollbackErr := pc.releaseLoadBalancerIP(*ip)
		if rollbackErr != nil {
			err = fmt.Errorf("%v: error rolling back IP address: %v", err, rollbackErr)
		}
		return nil, err
	}
	return ip, nil
}

func matchAllTags(csTags []cloudstack.Tags, tags map[string]string) bool {
	validCount := 0
	for _, tag := range csTags {
		if tags[tag.Key] == tag.Value {
			validCount++
			if validCount == len(tags) {
				return true
			}
		}
	}
	return false
}

// tryPublicIPAddressByTags tries retrieving an ip address matching service
// tags. If not is found it returns nil with no error.
func (pc *projectCloud) tryPublicIPAddressByTags(service *v1.Service) (*cloudstackIP, error) {
	klog.V(4).Infof("tryPublicIPAddressByTags(%v, %v)", service.Namespace, service.Name)
	client, err := pc.getClient()
	if err != nil {
		return nil, err
	}
	tags := tagsForService(service)

	p := client.Address.NewListPublicIpAddressesParams()
	p.SetListall(true)
	p.SetTags(tags)
	if pc.projectID != "" {
		p.SetProjectid(pc.projectID)
	}

	publicIPAddresses, err := listAllIPPages(client, p)
	if err != nil {
		return nil, fmt.Errorf("error retrieving IP address: %v", err)
	}

	var validIPs []cloudstackIP
	for _, publicIP := range publicIPAddresses {
		// This match call is necessary because aparently cloudstack does an OR
		// when multiple tags are specified and we want an AND.
		if matchAllTags(publicIP.Tags, tags) {
			validIPs = append(validIPs, cloudstackIP{
				id:      publicIP.Id,
				address: publicIP.Ipaddress,
			})
		}
	}

	if len(validIPs) == 0 {
		return nil, nil
	}

	if len(validIPs) > 1 {
		return nil, fmt.Errorf("multiple IP addresses for service %v/%v", service.Namespace, service.Name)
	}

	return &validIPs[0], nil
}

// getPublicIPAddressID retrieves an IP address by its address, if none is
// found an error is returned.
func (pc *projectCloud) getPublicIPAddressByIP(loadBalancerIP string) (*cloudstackIP, error) {
	klog.V(4).Infof("getPublicIPAddressByIP(%v)", loadBalancerIP)
	client, err := pc.getClient()
	if err != nil {
		return nil, err
	}
	p := client.Address.NewListPublicIpAddressesParams()
	p.SetIpaddress(loadBalancerIP)
	p.SetListall(true)

	if pc.projectID != "" {
		p.SetProjectid(pc.projectID)
	}

	l, err := client.Address.ListPublicIpAddresses(p)
	if err != nil {
		return nil, fmt.Errorf("error retrieving IP address: %v", err)
	}

	if l.Count == 0 {
		return nil, fmt.Errorf("could not find IP address %v", loadBalancerIP)
	}

	if l.Count > 1 {
		return nil, fmt.Errorf("multiple IP address found for %v", loadBalancerIP)
	}

	publicIP := l.PublicIpAddresses[0]
	if publicIP.State == "Allocated" {
		return nil, fmt.Errorf("unable to use IP %v, it's already allocated: %#v", loadBalancerIP, publicIP)
	}

	return &cloudstackIP{
		address: publicIP.Ipaddress,
		id:      publicIP.Id,
	}, nil
}

func (pc *projectCloud) getPublicIPAddressByID(ipID string) (*cloudstack.PublicIpAddress, error) {
	klog.V(4).Infof("getPublicIPAddressByID(%v)", ipID)
	client, err := pc.getClient()
	if err != nil {
		return nil, err
	}
	p := client.Address.NewListPublicIpAddressesParams()
	p.SetId(ipID)
	p.SetListall(true)

	if pc.projectID != "" {
		p.SetProjectid(pc.projectID)
	}

	l, err := client.Address.ListPublicIpAddresses(p)
	if err != nil {
		return nil, fmt.Errorf("error retrieving IP ID %v: %v", ipID, err)
	}

	if l.Count == 0 {
		return nil, fmt.Errorf("could not find IP ID %v", ipID)
	}

	if l.Count > 1 {
		return nil, fmt.Errorf("multiple IP ID found for %v", ipID)
	}

	return l.PublicIpAddresses[0], nil
}

// associatePublicIPAddress associates a new IP and sets the address and it's ID.
func (pc *projectCloud) associatePublicIPAddress(service *v1.Service, networkID string) (*cloudstackIP, error) {
	klog.V(4).Infof("Allocate new IP for service (%v, %v)", service.Namespace, service.Name)
	// If a network belongs to a VPC, the IP address needs to be associated with
	// the VPC instead of with the network.
	client, err := pc.getClient()
	if err != nil {
		return nil, err
	}
	network, count, err := client.Network.GetNetworkByID(networkID, cloudstack.WithProject(pc.projectID))
	if err != nil {
		if count == 0 {
			return nil, fmt.Errorf("could not find network %v", networkID)
		}
		return nil, fmt.Errorf("error retrieving network: %v", err)
	}

	params := &cloudstack.CustomServiceParams{}
	if network.Vpcid != "" {
		params.SetParam("vpcid", network.Vpcid)
	} else {
		params.SetParam("networkid", networkID)
	}
	if pc.projectID != "" {
		params.SetParam("projectid", pc.projectID)
	}
	environmentID := pc.getLBEnvironmentID()
	if environmentID != "" {
		params.SetParam("lbenvironmentid", environmentID)
	}

	var result cloudstack.AssociateIpAddressResponse
	associateCommand := pc.config.Command.AssociateIP
	if associateCommand == "" {
		associateCommand = "associateIpAddress"
	}

	setExtraParams(service, associateIPAddressExtraParamPrefix, params)

	err = client.Custom.CustomRequest(associateCommand, params, &result)
	if err != nil {
		return nil, fmt.Errorf("error associate new IP address using endpoint %q: %v", associateCommand, err)
	}

	ip := cloudstackIP{
		id:      result.Id,
		address: result.Ipaddress,
	}
	if result.JobID != "" {
		klog.V(4).Infof("Querying async job %s for load balancer %s", result.JobID, ip)
		err = waitJob(client, result.JobID, &result)
		if err != nil {
			return nil, err
		}
		ip.address = result.Ipaddress
	}
	klog.V(4).Infof("Allocated IP %s for service (%v, %v)", ip, service.Namespace, service.Name)

	return &ip, nil
}

// releasePublicIPAddress releases an associated IP.
func (pc *projectCloud) releaseLoadBalancerIP(ip cloudstackIP) error {
	klog.V(4).Infof("Release IP %s", ip)
	client, err := pc.getClient()
	if err != nil {
		return err
	}
	params := &cloudstack.CustomServiceParams{}
	params.SetParam("id", ip.id)
	if pc.projectID != "" {
		params.SetParam("projectid", pc.projectID)
	}

	disassociateCommand := pc.config.Command.DisassociateIP
	if disassociateCommand == "" {
		disassociateCommand = "disassociateIpAddress"
	}
	var rsp cloudstack.DisassociateIpAddressResponse
	err = client.Custom.CustomRequest(disassociateCommand, params, &rsp)
	if err != nil {
		return fmt.Errorf("error disassociate IP address using endpoint %q: %v", disassociateCommand, err)
	}
	if rsp.JobID != "" {
		return waitJob(client, rsp.JobID, nil)
	}
	return nil
}

func sortPorts(ports []v1.ServicePort) {
	sort.Slice(ports, func(i, j int) bool {
		return ports[i].Port < ports[j].Port
	})
}

func comparePorts(service *v1.Service, rule *loadBalancerRule, lb *loadBalancer) (bool, error) {
	ports := service.Spec.Ports
	_, useTargetPort := getLabelOrAnnotation(service.ObjectMeta, lbUseTargetPort)
	sortPorts(ports)
	var additionalPorts []string
	var targetPort int
	var err error
	for _, p := range ports[1:] {
		if useTargetPort {
			targetPort, err = lb.getTargetPort(p.TargetPort, service)
			if err != nil {
				return false, fmt.Errorf("Error resolving target port: %v", err)
			}
			additionalPorts = append(additionalPorts, fmt.Sprintf("%d:%d", p.Port, targetPort))
		} else {
			additionalPorts = append(additionalPorts, fmt.Sprintf("%d:%d", p.Port, p.NodePort))
		}
	}
	sort.Strings(additionalPorts)
	sort.Strings(rule.AdditionalPortMap)
	if len(rule.AdditionalPortMap) == 0 {
		rule.AdditionalPortMap = nil
	}
	if useTargetPort {
		targetPort, err = lb.getTargetPort(ports[0].TargetPort, service)
		if err != nil {
			return false, fmt.Errorf("Error resolving target port: %v", err)
		}
		return reflect.DeepEqual(additionalPorts, rule.AdditionalPortMap) &&
			rule.Privateport == strconv.Itoa(targetPort) &&
			rule.Publicport == strconv.Itoa(int(ports[0].Port)), nil
	}
	return reflect.DeepEqual(additionalPorts, rule.AdditionalPortMap) &&
		rule.Privateport == strconv.Itoa(int(ports[0].NodePort)) &&
		rule.Publicport == strconv.Itoa(int(ports[0].Port)), nil
}

// checkLoadBalancerRule checks if the rule already exists and if it does, if it can be updated. If
// it does exist but cannot be updated, it will delete the existing rule so it can be created again.
func (lb *loadBalancer) checkLoadBalancerRule(lbRuleName string, service *v1.Service) (bool, bool, error) {
	ports := service.Spec.Ports
	if lb.rule == nil {
		return false, false, nil
	}
	if len(ports) == 0 {
		return false, false, errors.New("invalid ports")
	}

	// Check if any of the values we cannot update (those that require a new
	// load balancer rule) are changed.
	portDiff, err := comparePorts(service, lb.rule, lb)
	if err != nil {
		return false, false, err
	}
	if portDiff && lb.name == lb.rule.Name {
		missingTags, err := lb.hasMissingTags()
		if err != nil {
			return false, false, err
		}
		needsUpdate := lb.rule.Algorithm != lb.algorithm || missingTags
		if needsUpdate {
			klog.V(4).Infof("checkLoadBalancerRule found differences, needsUpdate true for LB %s: rule: %#v, rule.LoadBalancerRule: %#v, lb: %#v, ports: %#v", lb.name, lb.rule, lb.rule.LoadBalancerRule, lb, ports)
		}
		return true, needsUpdate, nil
	}

	klog.V(4).Infof("checkLoadBalancerRule found differences, will delete LB %s: rule: %#v, rule.LoadBalancerRule: %#v, lb: %#v, ports: %#v", lb.rule.Name, lb.rule, lb.rule.LoadBalancerRule, lb, ports)

	// Delete the load balancer rule so we can create a new one using the new values.
	if err := lb.deleteLoadBalancerRule(); err != nil {
		return false, false, err
	}

	return false, false, nil
}

// updateLoadBalancerRule updates a load balancer rule.
func (lb *loadBalancer) updateLoadBalancerRule(service *v1.Service) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}

	p := client.LoadBalancer.NewUpdateLoadBalancerRuleParams(lb.rule.Id)
	p.SetAlgorithm(lb.algorithm)

	_, err = client.LoadBalancer.UpdateLoadBalancerRule(p)
	if err != nil {
		return err
	}

	return lb.cloud.assignTagsToRule(lb.rule, service)
}

// createLoadBalancerRule creates a new load balancer rule and returns it's ID.
func (lb *loadBalancer) createLoadBalancerRule(lbRuleName string, service *v1.Service) (*loadBalancerRule, error) {
	ports := service.Spec.Ports
	client, err := lb.getClient()
	var targetPort int
	if err != nil {
		return nil, err
	}
	if len(ports) == 0 {
		return nil, errors.New("missing ports")
	}

	sortPorts(ports)

	p := &cloudstack.CustomServiceParams{}
	p.SetParam("algorithm", lb.algorithm)
	p.SetParam("name", lbRuleName)
	p.SetParam("privateport", int(ports[0].NodePort))
	_, useTargetPort := getLabelOrAnnotation(service.ObjectMeta, lbUseTargetPort)
	if useTargetPort {
		if targetPort, err = lb.getTargetPort(ports[0].TargetPort, service); err != nil {
			return nil, fmt.Errorf("error getting target port: %v", err)
		}
		p.SetParam("privateport", targetPort)
	}
	p.SetParam("publicport", int(ports[0].Port))
	p.SetParam("networkid", lb.mainNetworkID)
	p.SetParam("publicipid", lb.ip.id)

	switch ports[0].Protocol {
	case v1.ProtocolTCP:
		p.SetParam("protocol", "TCP")
	case v1.ProtocolUDP:
		p.SetParam("protocol", "UDP")
	default:
		return nil, fmt.Errorf("unsupported load balancer protocol: %v", ports[0].Protocol)
	}

	var additionalPorts []string
	for _, p := range ports[1:] {
		if useTargetPort {
			if targetPort, err = lb.getTargetPort(p.TargetPort, service); err != nil {
				return nil, fmt.Errorf("error getting target port: %v", err)
			}
			additionalPorts = append(additionalPorts, fmt.Sprintf("%d:%d", p.Port, targetPort))
		} else {
			additionalPorts = append(additionalPorts, fmt.Sprintf("%d:%d", p.Port, p.NodePort))
		}
	}
	if len(additionalPorts) > 0 {
		p.SetParam("additionalportmap", strings.Join(additionalPorts, ","))
	}

	// Do not create corresponding firewall rule.
	p.SetParam("openfirewall", false)

	setExtraParams(service, createLoadBalancerExtraParamPrefix, p)

	// Create a new load balancer rule.
	r := cloudstack.CreateLoadBalancerRuleResponse{}

	err = client.Custom.CustomRequest("createLoadBalancerRule", p, &r)
	if err != nil {
		return nil, fmt.Errorf("error creating load balancer rule %v: %v", lbRuleName, err)
	}
	if r.JobID != "" {
		err = waitJob(client, r.JobID, &r)
		if err != nil {
			return nil, fmt.Errorf("error waiting for load balancer rule job %v: %v", lbRuleName, err)
		}
	}

	lbRule := &loadBalancerRule{
		LoadBalancerRule: &cloudstack.LoadBalancerRule{
			Id:          r.Id,
			Algorithm:   r.Algorithm,
			Cidrlist:    r.Cidrlist,
			Name:        r.Name,
			Networkid:   r.Networkid,
			Privateport: r.Privateport,
			Publicport:  r.Publicport,
			Publicip:    r.Publicip,
			Publicipid:  r.Publicipid,
			Zoneid:      r.Zoneid,
		},
	}

	return lbRule, nil
}

func (lb *loadBalancer) updateLoadBalancerPool(lbRule *loadBalancerRule, service *v1.Service) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}

	_, lbCustomHealthCheckVal := getLabelOrAnnotation(service.ObjectMeta, lbCustomHealthCheck)
	if !lbCustomHealthCheckVal {
		return nil
	}

	listGloboNetworkPoolsParams := cloudstack.CustomServiceParams{}
	listGloboNetworkPoolsResponse := globoNetworkPools{}
	listGloboNetworkPoolsParams.SetParam("lbruleid", lbRule.Id)
	listGloboNetworkPoolsParams.SetParam("zoneid", lbRule.Zoneid)

	err = client.Custom.CustomRequest("listGloboNetworkPools", &listGloboNetworkPoolsParams, &listGloboNetworkPoolsResponse)

	if err != nil {
		return fmt.Errorf("error list load balancer pools for %v: %v", lbRule.Name, err)
	}

	if listGloboNetworkPoolsResponse.Count < 1 {
		return fmt.Errorf("error list load balancer pools for %v: no LB pools found", lbRule.Name)
	}

	updateGloboNetworkPoolsParams := cloudstack.CustomServiceParams{}
	r := UpdateGloboNetworkPoolResponse{}
	for idx := range service.Spec.Ports {
		pool, err := lb.generateGloboNetworkPool(idx, service, listGloboNetworkPoolsResponse.GloboNetworkPools)
		if err != nil {
			return fmt.Errorf("error waiting for load balancer rule job %v: %v", lbRule.Name, err)
		}
		if pool == nil {
			continue
		}
		updateGloboNetworkPoolsParams.SetParam("poolids", pool.Id)
		updateGloboNetworkPoolsParams.SetParam("lbruleid", lbRule.Id)
		updateGloboNetworkPoolsParams.SetParam("healthchecktype", strings.ToUpper(pool.HealthCheckType))
		updateGloboNetworkPoolsParams.SetParam("healthcheck", pool.HealthCheck)
		updateGloboNetworkPoolsParams.SetParam("expectedhealthcheck", pool.HealthCheckExpected)
		updateGloboNetworkPoolsParams.SetParam("zoneid", lbRule.Zoneid)
		updateGloboNetworkPoolsParams.SetParam("maxconn", 0)
		err = client.Custom.CustomRequest("updateGloboNetworkPool", &updateGloboNetworkPoolsParams, &r)
		if err != nil {
			return fmt.Errorf("error updating globo network pool for %v: %v", lbRule.Name, err)
		}
		if r.JobID != "" {
			err = waitJob(client, r.JobID, &r)
			if err != nil {
				return fmt.Errorf("error waiting for globo network pool for rule %v: %v", lbRule.Name, err)
			}
		}
	}
	return nil
}

func (lb *loadBalancer) generateGloboNetworkPool(portsIdx int, service *v1.Service, globoPools []*globoNetworkPool) (*globoNetworkPool, error) {
	var err error
	_, useTargetPort := getLabelOrAnnotation(service.ObjectMeta, lbUseTargetPort)
	ports := service.Spec.Ports
	dstPort := int(ports[portsIdx].NodePort)
	vipPort := int(ports[portsIdx].Port)

	if useTargetPort {
		if dstPort, err = lb.getTargetPort(ports[portsIdx].TargetPort, service); err != nil {
			return nil, err
		}
	}

	if ports[portsIdx].Name == "" {
		return nil, nil
	}

	namedService := ports[portsIdx].Name
	protocol := strings.Split(namedService, "-")[0]
	healthCheckResponse, _ := getLabelOrAnnotation(service.ObjectMeta, fmt.Sprintf("%s%s", lbCustomHealthCheckResponsePrefix, namedService))
	healthCheckMessage, _ := getLabelOrAnnotation(service.ObjectMeta, fmt.Sprintf("%s%s", lbCustomHealthCheckMessagePrefix, namedService))
	if healthCheckMessage == "" || healthCheckResponse == "" {
		return nil, nil
	}

	for _, pool := range globoPools {
		if (pool.VipPort == vipPort && pool.Port == dstPort) &&
			(pool.HealthCheck != healthCheckMessage ||
				pool.HealthCheckExpected != healthCheckResponse ||
				pool.HealthCheckType != protocol) {
			pool.HealthCheck = healthCheckMessage
			pool.HealthCheckExpected = healthCheckResponse
			pool.HealthCheckType = protocol
			return pool, nil
		}
	}
	return nil, nil
}

// deleteLoadBalancerRule deletes a load balancer rule.
func (lb *loadBalancer) deleteLoadBalancerRule() error {
	klog.V(4).Infof("Deleting load balancer rule: %v", lb.rule.Id)
	client, err := lb.getClient()
	if err != nil {
		return err
	}

	deleteLBCommand := lb.cloud.config.Command.DeleteLBRule
	if deleteLBCommand == "" {
		deleteLBCommand = "deleteLoadBalancerRule"
	}

	p := &cloudstack.CustomServiceParams{}
	p.SetParam("id", lb.rule.Id)
	for k, v := range lb.cloud.config.CommandArgs[deleteLBCommand].ToMap() {
		p.SetParam(k, v)
	}

	var result cloudstack.DeleteLoadBalancerRuleResponse
	err = client.Custom.CustomRequest(deleteLBCommand, p, &result)
	if err != nil {
		return fmt.Errorf("error deleting load balancer rule %v: %v", lb.rule.Id, err)
	}

	if result.JobID != "" {
		err = waitJob(client, result.JobID, nil)
		if err != nil {
			return err
		}
	}

	lb.rule = nil
	return nil
}

// shouldManageIP checks if a IP has the provider tag and the corresponding service tags
func shouldManageIP(ip cloudstack.PublicIpAddress, service *v1.Service) bool {
	wantedTags := tagsForService(service)
	for tagKey, wantedValue := range wantedTags {
		value, isTagSet := getTag(ip.Tags, tagKey)
		if !isTagSet || wantedValue != value {
			klog.V(3).Infof("should NOT manage IP %s/%s. Expected value for tag %q: %q, got: %q.", ip.Id, ip.Ipaddress, tagKey, wantedValue, value)
			return false
		}
	}
	return true
}

// shouldManageLB checks if LB has the provider tag and the corresponding service tags
func shouldManageLB(lb *loadBalancer, service *v1.Service) (bool, error) {
	if lb.rule == nil {
		return true, nil
	}
	wantedTags := tagsForService(service)
	optionalTags := map[string]struct{}{namespaceTag: {}}
	for tagKey, wantedValue := range wantedTags {
		value, isTagSet := getTag(lb.rule.Tags, tagKey)
		if _, isOptional := optionalTags[tagKey]; isOptional && !isTagSet {
			continue
		}
		if !isTagSet || wantedValue != value {
			klog.V(3).Infof("should NOT manage LB %s. Expected value for tag %q: %q, got: %q.", lb.name, tagKey, wantedValue, value)
			return false, nil
		}
	}
	return true, nil
}

func (lb *loadBalancer) hasMissingTags() (bool, error) {
	wantedTags := []string{cloudProviderTag, serviceTag, namespaceTag}
	tagMap := map[string]string{}
	for _, lbTag := range lb.rule.Tags {
		tagMap[lbTag.Key] = lbTag.Value
	}
	for _, t := range wantedTags {
		_, hasTag := tagMap[t]
		if !hasTag {
			return true, nil
		}
	}
	return false, nil
}

func getTag(tags []cloudstack.Tags, k string) (string, bool) {
	for _, tag := range tags {
		if tag.Key == k {
			return tag.Value, true
		}
	}
	return "", false
}

func (lb *loadBalancer) deleteTags(service *v1.Service) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	if lb.rule == nil {
		return nil
	}
	p := client.Resourcetags.NewDeleteTagsParams([]string{lb.rule.Id}, CloudstackResourceLoadBalancer)
	p.SetTags(tagsForService(service))
	_, err = client.Resourcetags.DeleteTags(p)
	return err
}

func tagsForService(service *v1.Service) map[string]string {
	return map[string]string{
		cloudProviderTag: ProviderName,
		serviceTag:       service.Name,
		namespaceTag:     service.Namespace,
	}
}

func (pc *projectCloud) assignTagsToRule(lbRule *loadBalancerRule, service *v1.Service) error {
	return pc.setDefaultTags(CloudstackResourceLoadBalancer, lbRule.Id, service)
}

func (pc *projectCloud) assignTagsToIP(ip *cloudstackIP, service *v1.Service) error {
	return pc.setDefaultTags(CloudstackResourceIPAdress, ip.id, service)
}

func (pc *projectCloud) setDefaultTags(resourceType, resourceID string, service *v1.Service) error {
	return pc.setResourceTags(resourceType, resourceID, tagsForService(service))
}

func (pc *projectCloud) setResourceTags(resourceType, resourceID string, tags map[string]string) error {
	client, err := pc.getClient()
	if err != nil {
		return err
	}
	var orderedTags []string
	for k := range tags {
		orderedTags = append(orderedTags, k)
	}
	// Sort tags for deterministic API calls easing debugging
	sort.Strings(orderedTags)
	for _, k := range orderedTags {
		v := tags[k]
		// Creating one by one so that we can ignore tags that already exist.
		tp := client.Resourcetags.NewCreateTagsParams([]string{resourceID}, resourceType, map[string]string{
			k: v,
		})
		_, err = client.Resourcetags.CreateTags(tp)
		if err != nil {
			if !strings.Contains(err.Error(), "already exist") {
				return fmt.Errorf("error adding tags to %s %s: %v", resourceType, resourceID, err)
			}
		}
	}
	return nil
}

// assignHostsToRule assigns hosts to a load balancer rule.
func (lb *loadBalancer) assignHostsToRule(lbRule *loadBalancerRule, hostIDs []string) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	p := client.LoadBalancer.NewAssignToLoadBalancerRuleParams(lbRule.Id)
	p.SetVirtualmachineids(hostIDs)

	if _, err := client.LoadBalancer.AssignToLoadBalancerRule(p); err != nil {
		return fmt.Errorf("error assigning hosts to load balancer rule %v: %v", lbRule.Id, err)
	}

	return nil
}

// assignNetworksToRule assigns networks to a load balancer rule.
func (lb *loadBalancer) assignNetworksToRule(lbRule *loadBalancerRule, networkIDs []string) error {
	if lb.cloud.config.Command.AssignNetworks == "" {
		return nil
	}
	for i := range networkIDs {
		if err := lb.assignNetworkToRule(lbRule, networkIDs[i]); err != nil {
			return err
		}
	}
	return nil
}

func (lb *loadBalancer) assignNetworkToRule(lbRule *loadBalancerRule, networkID string) error {
	klog.V(4).Infof("assign network %q to rule %s", networkID, lbRule.Id)
	p := &cloudstack.CustomServiceParams{}
	if lb.cloud.projectID != "" {
		p.SetParam("projectid", lb.cloud.projectID)
	}
	p.SetParam("id", lbRule.Id)
	p.SetParam("networkids", []string{networkID})
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	var result struct {
		JobID string `json:"jobid"`
	}
	if err = client.Custom.CustomRequest(lb.cloud.config.Command.AssignNetworks, p, &result); err != nil {
		return fmt.Errorf("error assigning networks to load balancer rule %s using endpoint %q: %v ", lbRule.Name, lb.cloud.config.Command.AssignNetworks, err)
	}
	if result.JobID != "" {
		klog.V(4).Infof("Querying async job %s for load balancer rule %s", result.JobID, lbRule.Id)
		err = waitJob(client, result.JobID, nil)
		if err != nil {
			if !strings.Contains(err.Error(), "is already mapped") {
				// we ignore the error if is in the form `Network XXX is already mapped to load balancer`
				return err
			}
		}
	}
	return nil
}

// removeHostsFromRule removes hosts from a load balancer rule.
func (lb *loadBalancer) removeHostsFromRule(lbRule *loadBalancerRule, hostIDs []string) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	p := client.LoadBalancer.NewRemoveFromLoadBalancerRuleParams(lbRule.Id)
	p.SetVirtualmachineids(hostIDs)

	if _, err := client.LoadBalancer.RemoveFromLoadBalancerRule(p); err != nil {
		return fmt.Errorf("error removing hosts from load balancer rule %v: %v", lbRule.Name, err)
	}

	return nil
}

func (lb *loadBalancer) setAlgorithm(service *v1.Service) error {
	switch service.Spec.SessionAffinity {
	case v1.ServiceAffinityNone:
		lb.algorithm = "roundrobin"
	case v1.ServiceAffinityClientIP:
		lb.algorithm = "source"
	default:
		return fmt.Errorf("unsupported load balancer affinity: %v", service.Spec.SessionAffinity)
	}
	return nil
}

func (lb *loadBalancer) getClient() (*cloudstack.CloudStackClient, error) {
	return lb.cloud.getClient()
}

func (pc *projectCloud) getClient() (*cloudstack.CloudStackClient, error) {
	client := pc.environments[pc.environment].client
	if client == nil {
		return nil, fmt.Errorf("unable to retrieve cloudstack client for env: %#v", pc)
	}
	return client, nil
}

func (pc *projectCloud) getLBEnvironmentID() string {
	return pc.environments[pc.environment].lbEnvironmentID
}

func setExtraParams(service *v1.Service, prefix string, params *cloudstack.CustomServiceParams) {
	for key, value := range service.ObjectMeta.Annotations {
		if strings.HasPrefix(key, prefix) {
			params.SetParam(strings.TrimPrefix(key, prefix), value)
		}
	}

	for key, value := range service.ObjectMeta.Labels {
		if strings.HasPrefix(key, prefix) {
			params.SetParam(strings.TrimPrefix(key, prefix), value)
		}
	}
}

// symmetricDifference returns the symmetric difference between the old (existing) and new (wanted) host ID's.
func symmetricDifference(hostIDs []string, lbInstances []*cloudstack.VirtualMachine) ([]string, []string) {
	new := make(map[string]bool)
	for _, hostID := range hostIDs {
		new[hostID] = true
	}

	var remove []string
	for _, instance := range lbInstances {
		if new[instance.Id] {
			delete(new, instance.Id)
			continue
		}

		remove = append(remove, instance.Id)
	}

	var assign []string
	for hostID := range new {
		assign = append(assign, hostID)
	}

	return assign, remove
}

func waitJob(client *cloudstack.CloudStackClient, jobID string, result interface{}) error {
	pa := &cloudstack.QueryAsyncJobResultParams{}
	pa.SetJobID(jobID)
	data, err := client.GetAsyncJobResult(jobID, asyncJobWaitTimeout)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	firstData, err := getFirstRawValue(data)
	if err != nil {
		return err
	}
	err = json.Unmarshal(firstData, result)
	if err != nil {
		return json.Unmarshal(data, result)
	}
	return nil
}

func getFirstRawValue(raw json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for _, v := range m {
		return v, nil
	}
	return nil, fmt.Errorf("Unable to extract the raw value from:\n\n%s\n\n", string(raw))
}

func (lb *loadBalancer) getTargetPort(targetPort intstr.IntOrString, service *v1.Service) (int, error) {
	if targetPort.IntValue() > 0 {
		return targetPort.IntValue(), nil
	}
	endpoint, err := lb.cloud.kubeClient.CoreV1().Endpoints(service.Namespace).Get(service.Name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("error get endpoints: %v", err)
	}
	if len(endpoint.Subsets) < 1 {
		return 0, fmt.Errorf("no endpoints found for %s service on %s namespace", service.Name, service.Namespace)
	}
	ports := endpoint.Subsets[0].Ports
	for _, port := range ports {
		if port.Name == targetPort.String() {
			return int(port.Port), nil
		}
	}
	return 0, fmt.Errorf("no port name \"%s\" found for endpoint service %s on namespace %s", targetPort.StrVal, service.Name, service.Namespace)
}

func (lb *loadBalancer) syncNodes(hostIDs, networkIDs []string) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}

	p := client.LoadBalancer.NewListLoadBalancerRuleInstancesParams(lb.rule.Id)
	vms, err := listAllLBInstancesPages(client, p)
	if err != nil {
		return fmt.Errorf("error retrieving associated instances: %v", err)
	}

	assign, remove := symmetricDifference(hostIDs, vms)

	if len(assign) > 0 {
		klog.V(4).Infof("Assigning networks (%v) to load balancer rule: %v", networkIDs, lb.rule.Name)
		if err := lb.assignNetworksToRule(lb.rule, networkIDs); err != nil {
			return err
		}

		klog.V(4).Infof("Assigning new hosts (%v) to load balancer rule: %v", assign, lb.rule.Name)
		if err := lb.assignHostsToRule(lb.rule, assign); err != nil {
			return err
		}
	}

	if len(remove) > 0 {
		klog.V(4).Infof("Removing old hosts (%v) from load balancer rule: %v", assign, lb.rule.Name)
		if err := lb.removeHostsFromRule(lb.rule, remove); err != nil {
			return err
		}
	}
	return nil
}
