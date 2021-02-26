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
	"time"

	"github.com/xanzy/go-cloudstack/v2/cloudstack"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

	removeLBsOnDeleteLabelKey = "csccm.cloudprovider.io/remove-loadbalancers-on-delete"

	cloudProviderTag       = "cloudprovider"
	serviceTag             = "kubernetes_service"
	namespaceTag           = "kubernetes_namespace"
	cloudProviderIgnoreTag = "cloudprovider-ignore"

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
	service       *v1.Service
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
	HealthCheckExpected string `json:"healthcheckexpect"`
	Id                  int    `json:"id"`
}

type UpdateGloboNetworkPoolResponse struct {
	JobID     string `json:"jobid"`
	Jobstatus int    `json:"jobstatus"`
}

type IPNotFoundError struct {
	ip string
}

func (e IPNotFoundError) Error() string {
	return fmt.Sprintf("could not find IP address %v", e.ip)
}

func (lb *loadBalancer) String() string {
	if lb == nil {
		return "lb(nil)"
	}
	svcName := "nil"
	if lb.service != nil {
		svcName = fmt.Sprintf("%s/%s", lb.service.Namespace, lb.service.Name)
	}
	projID := "nil"
	if lb.cloud != nil && lb.cloud.projectID != "" {
		projID = lb.cloud.projectID
	}
	return fmt.Sprintf("lb(%v, %v, %v, %v, svc(%v))", lb.name, projID, lb.rule, lb.ip, svcName)
}

func (lb *loadBalancerRule) String() string {
	if lb == nil || lb.LoadBalancerRule == nil {
		return "lbrule(nil)"
	}
	return fmt.Sprintf("lbrule(%v, %v)", lb.LoadBalancerRule.Id, lb.LoadBalancerRule.Name)
}

func (ip cloudstackIP) String() string {
	return fmt.Sprintf("ip(%v, %v)", ip.id, ip.address)
}

func (ip cloudstackIP) isValid() bool {
	return ip.id != "" && ip.address != ""
}

// GetLoadBalancer returns whether the specified load balancer exists, and if so, what its status is.
func (cs *CSCloud) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	if service == nil {
		return nil, false, fmt.Errorf("GetLoadBalancer: service cannot be nil")
	}

	klog.V(4).Infof("GetLoadBalancer(%v, %v, %v)", clusterName, service.Namespace, service.Name)
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

	klog.V(4).Infof("Found a load balancer %v", lb)

	status := &v1.LoadBalancerStatus{}
	status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{
		IP:       lb.ip.address,
		Hostname: lb.name,
	})

	return status, true, nil
}

// EnsureLoadBalancer creates a new load balancer, or updates the existing one. Returns the status of the balancer.
func (cs *CSCloud) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	if service == nil {
		return nil, fmt.Errorf("EnsureLoadBalancer: service cannot be nil")
	}

	klog.V(4).Infof("EnsureLoadBalancer(%v, %v, %v, %v, ports: %d, nodes: %d)", clusterName, service.Namespace, service.Name, service.Spec.LoadBalancerIP, len(service.Spec.Ports), len(nodes))
	cs.svcLock.Lock(service)
	defer cs.svcLock.Unlock(service)

	if len(service.Spec.Ports) == 0 {
		return nil, fmt.Errorf("requested load balancer with no ports")
	}

	err := cs.nodeRegistry.updateNodes(nodes)
	if err != nil {
		return nil, err
	}

	_, networkIDs, projectID, err := cs.nodeRegistry.idsForService(service)
	if err != nil {
		return nil, err
	}

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, projectID, networkIDs)
	if err != nil {
		return nil, err
	}

	if lb.cloud.projectID != "" && cs.config.Global.ProjectIDLabel != "" && service.Labels[cs.config.Global.ProjectIDLabel] == "" {
		service.Labels[cs.config.Global.ProjectIDLabel] = lb.cloud.projectID

		_, err = cs.kubeClient.CoreV1().Services(service.Namespace).Patch(
			service.Name,
			types.JSONPatchType,
			createJSONPatchForLabel(cs.config.Global.ProjectIDLabel, lb.cloud.projectID),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to patch service with project-id label: %v", err)
		}
	}

	err = lb.setAlgorithm(service)
	if err != nil {
		return nil, err
	}

	klog.V(4).Infof("Ensuring Load Balancer: %v", lb)

	err = shouldManageLB(lb)
	if err != nil {
		klog.V(3).Infof("Skipping EnsureLoadBalancer for %v: %v", lb, err)
		return nil, err
	}

	err = lb.loadLoadBalancerIP()
	if err != nil {
		return nil, err
	}

	if service.Spec.LoadBalancerIP != "" && lb.ip.address != service.Spec.LoadBalancerIP {
		err = lb.updateLoadBalancerIP()
		if err != nil {
			return nil, err
		}
	}

	klog.V(4).Infof("Load balancer has associated IP %v", lb)

	// If the load balancer rule exists and is up-to-date, we move on to the next rule.
	result, err := lb.checkLoadBalancerRule()
	if err != nil {
		return nil, err
	}

	if result.needsUpdate {
		klog.V(4).Infof("Updating load balancer: %v", lb)
		if err = lb.updateLoadBalancerRule(); err != nil {
			return nil, err
		}
	}

	if result.needsTags {
		if err = lb.assignTagsToRule(); err != nil {
			return nil, err
		}
	}

	status := &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{{
			IP:       lb.ip.address,
			Hostname: lb.name,
		}},
	}

	if !result.exists {
		klog.V(4).Infof("Creating load balancer rule: %v", lb)
		lb.rule, err = lb.createLoadBalancerRule()
		if err != nil {
			return nil, err
		}

		klog.V(4).Infof("Assigning tag to load balancer rule: %v", lb)
		if err = lb.assignTagsToRule(); err != nil {
			return nil, err
		}
	}

	err = cs.updateLBQueue.push(queueEntry{
		service:    service,
		lb:         lb,
		start:      time.Now(),
		updatePool: true,
	})
	if err != nil {
		return nil, err
	}

	return status, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (cs *CSCloud) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	if service == nil {
		return fmt.Errorf("UpdateLoadBalancer: service cannot be nil")
	}

	klog.V(4).Infof("UpdateLoadBalancer(%v, %v, %v, %#v)", clusterName, service.Namespace, service.Name, nodes)

	err := cs.nodeRegistry.updateNodes(nodes)
	if err != nil {
		return err
	}

	return cs.updateLBQueue.push(queueEntry{
		service: service,
		start:   time.Now(),
	})
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it exists, returning
// nil if the load balancer specified either didn't exist or was successfully deleted.
func (cs *CSCloud) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	if service == nil {
		return fmt.Errorf("EnsureLoadBalancerDeleted: service cannot be nil")
	}

	klog.V(4).Infof("EnsureLoadBalancerDeleted(%v, %v, %v)", clusterName, service.Namespace, service.Name)
	cs.svcLock.Lock(service)
	defer cs.svcLock.Unlock(service)

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, "", nil)
	if err != nil {
		return err
	}

	if lb.rule == nil {
		klog.V(3).Infof("Skipping EnsureLoadBalancerDeleted; LoadBalancerRule not found for service %s/%s", service.Namespace, service.Name)
		return nil
	}

	if !isLBRemovalEnabled(lb, service) {
		klog.V(3).Infof("Skipping deletion of load balancer %s: service or environment has removals disabled.", lb)
		return nil
	}

	err = shouldManageLB(lb)
	if err != nil {
		klog.V(3).Infof("Skipping EnsureLoadBalancerDeleted for service %s/%s: %v", service.Namespace, service.Name, err)
		return nil
	}

	klog.V(4).Infof("Deleting load balancer rule: %v", lb)
	if err := lb.deleteLoadBalancerRule(); err != nil {
		return err
	}

	if lb.ip.id != "" {
		klog.V(4).Infof("Releasing load balancer IP: %v", lb)
		if err := lb.cloud.releaseIPIfManaged(lb.ip, service); err != nil {
			return err
		}
	}

	return nil
}

// isLBRemovalEnabled indicates whether the configurations for load balancer
// removal is enabled.
func isLBRemovalEnabled(lb *loadBalancer, service *v1.Service) bool {
	klog.V(4).Infof("isLBRemovalEnabled(%v, %v, %v)", lb, service.Namespace, service.Name)

	if removeLBFlag, ok := getLabelOrAnnotation(service.ObjectMeta, removeLBsOnDeleteLabelKey); ok {
		klog.V(4).Infof("LB removal flag %q has been found on %q Service labels/annotations", removeLBFlag, service)
		if b, err := strconv.ParseBool(removeLBFlag); err == nil {
			return b
		}
	}

	klog.V(4).Infof("checking whether the LB removal is enabled for the %q environment", lb.cloud.environment)
	return lb.cloud.environments[lb.cloud.environment].removeLBs
}

// getLoadBalancer retrieves the IP address and ID and all the existing rules it can find.
func (cs *CSCloud) getLoadBalancer(service *v1.Service, projectID string, networkIDs []string) (*loadBalancer, error) {
	environment := cs.environmentForMeta(service.ObjectMeta)
	if projectID == "" {
		var err error
		projectID, err = cs.projectForMeta(service.ObjectMeta, environment)
		if err != nil {
			klog.V(4).Infof("unable to retrieve projectID for service: %#v: %v", service, err)
		}
	}
	lb := &loadBalancer{
		cloud: &projectCloud{
			CSCloud:     cs,
			environment: environment,
			projectID:   projectID,
		},
		service: service,
		name:    cs.getLoadBalancerName(service),
	}
	if len(networkIDs) > 0 {
		lb.mainNetworkID = networkIDs[0]
	}

	client, err := lb.getClient()
	if err != nil {
		return nil, err
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
	// few lines down. Setting more tags would actually be worse than setting a
	// single one because cloudstack will OR the tags instead of AND.
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

// GetLoadBalancerName returns the name of the load balancer responsible for
// the service by looking at the service label. If not set, it fallsback to the
// concatanation of the service name and the environment load balancer domain
// for the environent
func (cs *CSCloud) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	if service == nil {
		return ""
	}

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
	environment := cs.environmentForMeta(service.ObjectMeta)
	var lbDomain string
	if env, ok := cs.environments[environment]; ok {
		lbDomain = env.lbDomain
	}
	return fmt.Sprintf("%s.%s", service.Name, lbDomain)
}

func (lb *loadBalancer) loadLoadBalancerIP() error {
	if lb.ip.isValid() {
		return nil
	}
	ip, err := lb.cloud.getLoadBalancerIP(lb.service, lb.mainNetworkID)
	if err != nil {
		return err
	}
	lb.ip = *ip
	return nil
}

func (lb *loadBalancer) updateLoadBalancerIP() error {
	publicIP, err := lb.cloud.getPublicIPAddressByIP(lb.service.Spec.LoadBalancerIP)
	if err != nil {
		return err
	}
	oldIP := lb.ip
	lb.ip = *publicIP

	if lb.rule != nil {
		err = lb.deleteLoadBalancerRule()
		if err != nil {
			return err
		}
		lb.rule = nil
	}

	if !oldIP.isValid() {
		return nil
	}

	return lb.cloud.releaseIPIfManaged(oldIP, lb.service)
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
// 2 - Find an existing public IP matching Status.LoadBalancer.Ingress[0].IP
// 3 - Find a public IP matching service's Spec.LoadBalancerIP
// 4 - Allocate a new random IP
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
	ingresses := service.Status.LoadBalancer.Ingress
	if len(ingresses) > 0 && ingresses[0].IP != "" {
		ip, err = pc.getPublicIPAddressByIP(ingresses[0].IP)
		if err != nil {
			if _, ok := err.(IPNotFoundError); !ok {
				return nil, err
			}
		}
	}
	if ip == nil {
		ip, err = pc.associatePublicIPAddress(service, networkID)
		if err != nil {
			return nil, err
		}
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
	p.SetTags(map[string]string{
		serviceTag: tags[serviceTag],
	})
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
		return nil, IPNotFoundError{ip: loadBalancerIP}
	}

	if l.Count > 1 {
		return nil, fmt.Errorf("multiple IP address found for %v", loadBalancerIP)
	}

	publicIP := l.PublicIpAddresses[0]
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
		klog.V(4).Infof("Querying async job %s for cmd %q for IP %v", result.JobID, associateCommand, ip)
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

func comparePorts(ports lbPorts, lb *loadBalancer) bool {
	rule := lb.rule
	sort.Strings(rule.AdditionalPortMap)
	if len(rule.AdditionalPortMap) == 0 {
		rule.AdditionalPortMap = nil
	}
	return reflect.DeepEqual(ports.additionalPorts(), rule.AdditionalPortMap) &&
		rule.Privateport == strconv.Itoa(ports.privatePort()) &&
		rule.Publicport == strconv.Itoa(ports.publicPort()) &&
		rule.Protocol == string(ports.protocol)
}

type checkLBResult struct {
	needsTags   bool
	needsUpdate bool
	exists      bool
}

// checkLoadBalancerRule checks if the rule already exists and if it does, if it can be updated. If
// it does exist but cannot be updated, it will delete the existing rule so it can be created again.
func (lb *loadBalancer) checkLoadBalancerRule() (checkLBResult, error) {
	result := checkLBResult{}
	if lb.rule == nil {
		return result, nil
	}

	// Check if any of the values we cannot update (those that require a new
	// load balancer rule) are changed.
	newPorts, err := serviceToLBPorts(lb)
	if err != nil {
		return result, err
	}
	portsEqual := comparePorts(newPorts, lb)

	if portsEqual && lb.name == lb.rule.Name {
		result.exists = true
		result.needsUpdate = lb.rule.Algorithm != lb.algorithm
		result.needsTags = lb.hasMissingTags()
		if result.needsUpdate {
			klog.V(4).Infof("checkLoadBalancerRule found differences for %v. existing algorithm: %v, new algorithm: %v", lb, lb.rule.Algorithm, lb.algorithm)
		}
		return result, nil
	}

	klog.V(4).Infof("checkLoadBalancerRule found differences for %v in ports, will delete LB. existing ports: %#v, new ports: %v", lb, lb.rule.LoadBalancerRule, newPorts)

	// Delete the load balancer rule so we can create a new one using the new values.
	err = lb.deleteLoadBalancerRule()
	if err != nil {
		return result, err
	}

	return result, nil
}

// updateLoadBalancerRule updates a load balancer rule.
func (lb *loadBalancer) updateLoadBalancerRule() error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}

	p := client.LoadBalancer.NewUpdateLoadBalancerRuleParams(lb.rule.Id)
	p.SetAlgorithm(lb.algorithm)

	_, err = client.LoadBalancer.UpdateLoadBalancerRule(p)
	if err != nil {
		return fmt.Errorf("unable to update load balancer %v: %v", lb, err)
	}

	return nil
}

// createLoadBalancerRule creates a new load balancer rule and returns it's ID.
func (lb *loadBalancer) createLoadBalancerRule() (*loadBalancerRule, error) {
	client, err := lb.getClient()
	if err != nil {
		return nil, err
	}

	ports, err := serviceToLBPorts(lb)
	if err != nil {
		return nil, err
	}

	p := &cloudstack.CustomServiceParams{}
	p.SetParam("algorithm", lb.algorithm)
	p.SetParam("name", lb.name)
	p.SetParam("privateport", ports.privatePort())
	p.SetParam("publicport", ports.publicPort())
	p.SetParam("networkid", lb.mainNetworkID)
	p.SetParam("publicipid", lb.ip.id)
	p.SetParam("protocol", string(ports.protocol))

	additionalPorts := ports.additionalPorts()
	if len(additionalPorts) > 0 {
		p.SetParam("additionalportmap", strings.Join(additionalPorts, ","))
	}

	// Do not create corresponding firewall rule.
	p.SetParam("openfirewall", false)

	setExtraParams(lb.service, createLoadBalancerExtraParamPrefix, p)

	// Create a new load balancer rule.
	r := cloudstack.CreateLoadBalancerRuleResponse{}

	err = client.Custom.CustomRequest("createLoadBalancerRule", p, &r)
	if err != nil {
		return nil, fmt.Errorf("error creating load balancer rule for %v: %v", lb, err)
	}
	if r.JobID != "" {
		err = waitJob(client, r.JobID, &r)
		if err != nil {
			return nil, fmt.Errorf("error waiting for load balancer rule job for %v: %v", lb, err)
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
			Protocol:    r.Protocol,
		},
	}

	return lbRule, nil
}

func (lb *loadBalancer) updateLoadBalancerPool() error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}

	_, lbCustomHealthCheckVal := getLabelOrAnnotation(lb.service.ObjectMeta, lbCustomHealthCheck)
	if !lbCustomHealthCheckVal && lb.rule.Protocol != string(v1.ProtocolUDP) {
		return nil
	}

	listGloboNetworkPoolsParams := cloudstack.CustomServiceParams{}
	listGloboNetworkPoolsResponse := globoNetworkPools{}
	listGloboNetworkPoolsParams.SetParam("lbruleid", lb.rule.Id)
	listGloboNetworkPoolsParams.SetParam("zoneid", lb.rule.Zoneid)

	err = client.Custom.CustomRequest("listGloboNetworkPools", &listGloboNetworkPoolsParams, &listGloboNetworkPoolsResponse)

	if err != nil {
		return fmt.Errorf("error list load balancer pools for %v: %v", lb, err)
	}

	if listGloboNetworkPoolsResponse.Count == 0 {
		return fmt.Errorf("error list load balancer pools for %v: no LB pools found", lb)
	}

	ports, err := serviceToLBPorts(lb)
	if err != nil {
		return err
	}

	updateGloboNetworkPoolsParams := cloudstack.CustomServiceParams{}
	r := UpdateGloboNetworkPoolResponse{}
	for _, portInfo := range ports.ports {
		pool := lb.generateGloboNetworkPool(ports, portInfo, lb.service, listGloboNetworkPoolsResponse.GloboNetworkPools)
		if pool == nil {
			continue
		}
		updateGloboNetworkPoolsParams.SetParam("poolids", pool.Id)
		updateGloboNetworkPoolsParams.SetParam("lbruleid", lb.rule.Id)
		updateGloboNetworkPoolsParams.SetParam("healthchecktype", strings.ToUpper(pool.HealthCheckType))
		updateGloboNetworkPoolsParams.SetParam("healthcheck", pool.HealthCheck)
		updateGloboNetworkPoolsParams.SetParam("expectedhealthcheck", pool.HealthCheckExpected)
		updateGloboNetworkPoolsParams.SetParam("zoneid", lb.rule.Zoneid)
		updateGloboNetworkPoolsParams.SetParam("maxconn", 0)

		if pool.HealthCheckType == string(v1.ProtocolUDP) {
			updateGloboNetworkPoolsParams.SetParam("l4protocol", strings.ToUpper(pool.HealthCheckType))
			updateGloboNetworkPoolsParams.SetParam("l7protocol", "Outros")
			updateGloboNetworkPoolsParams.SetParam("redeploy", true)
		}

		err = client.Custom.CustomRequest("updateGloboNetworkPool", &updateGloboNetworkPoolsParams, &r)
		if err != nil {
			return fmt.Errorf("error updating globo network pool for %v: %v", lb, err)
		}
		if r.JobID != "" {
			err = waitJob(client, r.JobID, &r)
			if err != nil {
				return fmt.Errorf("error waiting for globo network pool for rule for %v: %v", lb, err)
			}
		}
	}
	return nil
}

func (lb *loadBalancer) generateGloboNetworkPool(ports lbPorts, portInfo lbPortInfo, service *v1.Service, globoPools []*globoNetworkPool) *globoNetworkPool {
	dstPort := int(portInfo.privatePort)
	vipPort := int(portInfo.publicPort)
	namedService := portInfo.name
	hcProtocol := portToHCProtocol(ports, portInfo)
	healthCheckResponse, _ := getLabelOrAnnotation(service.ObjectMeta, fmt.Sprintf("%s%s", lbCustomHealthCheckResponsePrefix, namedService))
	healthCheckMessage, _ := getLabelOrAnnotation(service.ObjectMeta, fmt.Sprintf("%s%s", lbCustomHealthCheckMessagePrefix, namedService))

	if hcProtocol.requiresMsg && healthCheckMessage == "" {
		return nil
	}

	for _, pool := range globoPools {
		isPortPool := pool.VipPort == vipPort && pool.Port == dstPort
		if !isPortPool {
			continue
		}

		if pool.HealthCheck != healthCheckMessage ||
			pool.HealthCheckExpected != healthCheckResponse ||
			pool.HealthCheckType != hcProtocol.protocol {
			pool.HealthCheck = healthCheckMessage
			pool.HealthCheckExpected = healthCheckResponse
			pool.HealthCheckType = hcProtocol.protocol
			return pool
		}
	}
	return nil
}

type hcPortInfo struct {
	protocol    string
	requiresMsg bool
}

func portToHCProtocol(ports lbPorts, portInfo lbPortInfo) hcPortInfo {
	supportedHCProtocols := map[string]bool{
		"HTTP":  true,
		"HTTPS": true,
		"TCP":   false,
		"UDP":   false,
	}
	svcNamePrefix := strings.ToUpper(strings.Split(portInfo.name, "-")[0])
	if hasMsg, ok := supportedHCProtocols[svcNamePrefix]; ok {
		return hcPortInfo{protocol: svcNamePrefix, requiresMsg: hasMsg}
	}
	return hcPortInfo{protocol: string(ports.protocol), requiresMsg: false}
}

// deleteLoadBalancerRule deletes a load balancer rule.
func (lb *loadBalancer) deleteLoadBalancerRule() error {
	klog.V(4).Infof("Deleting load balancer rule: %v", lb)
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
		return fmt.Errorf("error deleting load balancer rule %v: %v", lb, err)
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
func shouldManageLB(lb *loadBalancer) error {
	if lb.rule == nil {
		return nil
	}

	_, ignoredSet := getTag(lb.rule.Tags, cloudProviderIgnoreTag)
	if ignoredSet {
		return fmt.Errorf("should not manage %v, tag %q is set", lb, cloudProviderIgnoreTag)
	}

	wantedTags := tagsForService(lb.service)
	optionalTags := map[string]struct{}{namespaceTag: {}}
	var missingTags []string
	for tagKey, wantedValue := range wantedTags {
		value, isTagSet := getTag(lb.rule.Tags, tagKey)
		if _, isOptional := optionalTags[tagKey]; isOptional && !isTagSet {
			continue
		}
		if !isTagSet || wantedValue != value {
			missingTags = append(missingTags, fmt.Sprintf("tag %q: expected: %q, got: %q", tagKey, wantedValue, value))
		}
	}

	if len(missingTags) == 0 {
		return nil
	}

	var statusHostname string

	ingresses := lb.service.Status.LoadBalancer.Ingress
	if len(ingresses) == 1 {
		statusHostname = ingresses[0].Hostname
		if statusHostname == lb.name {
			return nil
		}
	}

	sort.Strings(missingTags)
	return fmt.Errorf("should not manage %v - status hostname: %q - missing tags: %s", lb, statusHostname, strings.Join(missingTags, " - "))
}

func (lb *loadBalancer) hasMissingTags() bool {
	wantedTags := []string{cloudProviderTag, serviceTag, namespaceTag}
	tagMap := map[string]string{}
	for _, lbTag := range lb.rule.Tags {
		tagMap[lbTag.Key] = lbTag.Value
	}
	for _, t := range wantedTags {
		_, hasTag := tagMap[t]
		if !hasTag {
			return true
		}
	}
	return false
}

func getTag(tags []cloudstack.Tags, k string) (string, bool) {
	for _, tag := range tags {
		if tag.Key == k {
			return tag.Value, true
		}
	}
	return "", false
}

func tagsForService(service *v1.Service) map[string]string {
	return map[string]string{
		cloudProviderTag: ProviderName,
		serviceTag:       service.Name,
		namespaceTag:     service.Namespace,
	}
}

func (lb *loadBalancer) assignTagsToRule() error {
	return lb.cloud.setDefaultTags(CloudstackResourceLoadBalancer, lb.rule.Id, lb.service)
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
func (lb *loadBalancer) assignHostsToRule(hostIDs []string) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	p := client.LoadBalancer.NewAssignToLoadBalancerRuleParams(lb.rule.Id)
	p.SetVirtualmachineids(hostIDs)

	if _, err := client.LoadBalancer.AssignToLoadBalancerRule(p); err != nil {
		return fmt.Errorf("error assigning hosts to %v: %v", lb, err)
	}

	return nil
}

// assignNetworksToRule assigns networks to a load balancer rule.
func (lb *loadBalancer) assignNetworksToRule(networkIDs []string) error {
	if lb.cloud.config.Command.AssignNetworks == "" {
		return nil
	}
	for i := range networkIDs {
		if err := lb.assignNetworkToRule(networkIDs[i]); err != nil {
			return err
		}
	}
	return nil
}

func (lb *loadBalancer) assignNetworkToRule(networkID string) error {
	klog.V(4).Infof("assign network %q to %v", networkID, lb)
	p := &cloudstack.CustomServiceParams{}
	if lb.cloud.projectID != "" {
		p.SetParam("projectid", lb.cloud.projectID)
	}
	p.SetParam("id", lb.rule.Id)
	p.SetParam("networkids", []string{networkID})
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	var result struct {
		JobID string `json:"jobid"`
	}
	if err = client.Custom.CustomRequest(lb.cloud.config.Command.AssignNetworks, p, &result); err != nil {
		return fmt.Errorf("error assigning networks to %v using cmd %q: %v ", lb, lb.cloud.config.Command.AssignNetworks, err)
	}
	if result.JobID != "" {
		klog.V(4).Infof("Querying async job %s for cmd %q for load balancer %v", result.JobID, lb.cloud.config.Command.AssignNetworks, lb)
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
func (lb *loadBalancer) removeHostsFromRule(hostIDs []string) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	p := client.LoadBalancer.NewRemoveFromLoadBalancerRuleParams(lb.rule.Id)
	p.SetVirtualmachineids(hostIDs)

	if _, err := client.LoadBalancer.RemoveFromLoadBalancerRule(p); err != nil {
		return fmt.Errorf("error removing hosts from %v: %v", lb, err)
	}

	return nil
}

func (lb *loadBalancer) setAlgorithm(service *v1.Service) error {
	switch service.Spec.SessionAffinity {
	case v1.ServiceAffinityNone, "":
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
	return nil, fmt.Errorf("unable to extract the raw value from: %q", string(raw))
}

func (lb *loadBalancer) getTargetPort(targetPort intstr.IntOrString) (int, error) {
	if targetPort.IntValue() > 0 {
		return targetPort.IntValue(), nil
	}
	endpoint, err := lb.cloud.kubeClient.CoreV1().Endpoints(lb.service.Namespace).Get(lb.service.Name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("error get endpoints: %v", err)
	}
	if len(endpoint.Subsets) == 0 {
		return 0, fmt.Errorf("no endpoints found for %v", lb)
	}
	ports := endpoint.Subsets[0].Ports
	for _, port := range ports {
		if port.Name == targetPort.String() {
			return int(port.Port), nil
		}
	}
	return 0, fmt.Errorf("no port name \"%s\" found for endpoint for %v", targetPort.String(), lb)
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
		klog.V(4).Infof("Assigning networks (%v) to load balancer: %v", networkIDs, lb)
		if err := lb.assignNetworksToRule(networkIDs); err != nil {
			return err
		}

		klog.V(4).Infof("Assigning new hosts (%v) to load balancer: %v", assign, lb)
		if err := lb.assignHostsToRule(assign); err != nil {
			return err
		}
	}

	if len(remove) > 0 {
		klog.V(4).Infof("Removing old hosts (%v) from load balancer: %v", assign, lb)
		if err := lb.removeHostsFromRule(remove); err != nil {
			return err
		}
	}
	return nil
}

func nodeNames(nodes []*v1.Node) string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return strings.Join(names, ",")
}

var rfc6901Encoder = strings.NewReplacer("~", "~0", "/", "~1")

func createJSONPatchForLabel(key, value string) []byte {
	const format = `[
		{
			"op": "add",
			"path": "/metadata/labels/%s",
			"value": "%s"
		}
	]`
	return []byte(fmt.Sprintf(format, rfc6901Encoder.Replace(key), value))
}

func serviceToLBPorts(lb *loadBalancer) (lbPorts, error) {
	ports := lb.service.Spec.Ports
	if len(ports) == 0 {
		return lbPorts{}, errors.New("service ports are empty")
	}
	sortPorts(ports)

	protocol := ports[0].Protocol
	if protocol != v1.ProtocolTCP && protocol != v1.ProtocolUDP {
		return lbPorts{}, fmt.Errorf("unsupported load balancer protocol: %v", protocol)
	}

	result := lbPorts{
		protocol: protocol,
	}

	_, useTargetPort := getLabelOrAnnotation(lb.service.ObjectMeta, lbUseTargetPort)
	for _, p := range ports {
		if p.Protocol != protocol {
			return lbPorts{}, fmt.Errorf("unsupported load balancer with multiple protocols: %q and %q", protocol, p.Protocol)
		}
		targetPort := int(p.NodePort)
		if useTargetPort {
			var err error
			targetPort, err = lb.getTargetPort(p.TargetPort)
			if err != nil {
				return lbPorts{}, err
			}
		}
		result.ports = append(result.ports, lbPortInfo{
			publicPort:  int(p.Port),
			privatePort: targetPort,
			name:        p.Name,
		})
	}
	return result, nil
}

func sortPorts(ports []v1.ServicePort) {
	sort.Slice(ports, func(i, j int) bool {
		return ports[i].Port < ports[j].Port
	})
}

type lbPortInfo struct {
	publicPort  int
	privatePort int
	name        string
}

type lbPorts struct {
	ports    []lbPortInfo
	protocol v1.Protocol
}

func (p *lbPorts) publicPort() int {
	if len(p.ports) == 0 {
		return 0
	}
	return p.ports[0].publicPort
}

func (p *lbPorts) privatePort() int {
	if len(p.ports) == 0 {
		return 0
	}
	return p.ports[0].privatePort
}

func (p *lbPorts) additionalPorts() []string {
	if len(p.ports) < 2 {
		return nil
	}
	var additional []string
	for _, port := range p.ports[1:] {
		additional = append(additional, fmt.Sprintf("%d:%d", port.publicPort, port.privatePort))
	}
	sort.Strings(additional)
	return additional
}
