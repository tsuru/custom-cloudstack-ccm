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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/xanzy/go-cloudstack/cloudstack"
	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/cloudprovider"
)

const (
	lbNameLabel = "csccm.cloudprovider.io/loadbalancer-name"

	cloudProviderTag = "cloudprovider"
	serviceTag       = "kubernetes_service"
)

type loadBalancer struct {
	*CSCloud

	name        string
	algorithm   string
	hostIDs     []string
	ipAddr      string
	ipAddrID    string
	networkIDs  []string
	projectID   string
	environment string
	rule        *loadBalancerRule
}

type loadBalancerRule struct {
	*cloudstack.LoadBalancerRule
	AdditionalPortMap []string `json:"additionalportmap"`
}

// GetLoadBalancer returns whether the specified load balancer exists, and if so, what its status is.
func (cs *CSCloud) GetLoadBalancer(clusterName string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	glog.V(5).Infof("GetLoadBalancer(%v, %v, %v)", clusterName, service.Namespace, service.Name)

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, "")
	if err != nil {
		return nil, false, err
	}

	// If we don't have a rule, the load balancer does not exist.
	if lb.rule == nil {
		return nil, false, nil
	}

	glog.V(4).Infof("Found a load balancer associated with IP %v", lb.ipAddr)

	status := &v1.LoadBalancerStatus{}
	status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: lb.ipAddr})

	return status, true, nil
}

// EnsureLoadBalancer creates a new load balancer, or updates the existing one. Returns the status of the balancer.
func (cs *CSCloud) EnsureLoadBalancer(clusterName string, service *v1.Service, nodes []*v1.Node) (status *v1.LoadBalancerStatus, err error) {
	glog.V(5).Infof("EnsureLoadBalancer(%v, %v, %v, %v, %v, %#v)", clusterName, service.Namespace, service.Name, service.Spec.LoadBalancerIP, service.Spec.Ports, nodes)

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

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, projectID)
	if err != nil {
		return nil, err
	}
	lb.hostIDs = hostIDs
	lb.networkIDs = networkIDs

	lb.setAlgorithm(service)

	glog.V(4).Infof("Ensuring Load Balancer: %#v", lb)

	if !lb.hasLoadBalancerIP() {
		// Create or retrieve the load balancer IP.
		if errLB := lb.getLoadBalancerIP(service.Spec.LoadBalancerIP); errLB != nil {
			return nil, errLB
		}

		if lb.ipAddr != "" && lb.ipAddr != service.Spec.LoadBalancerIP {
			defer func(lb *loadBalancer) {
				if err != nil {
					if releaseErr := lb.releaseLoadBalancerIP(); releaseErr != nil {
						glog.Errorf(releaseErr.Error())
					}
				}
			}(lb)
		}
	} else {
		var manage bool
		manage, err = shouldManageLB(lb, service)
		if err != nil {
			return nil, fmt.Errorf("failed to check if LB should be managed: %v", err)
		}
		if !manage {
			glog.V(3).Infof("Skipping EnsureLoadBalancer for service %s and LB %s", service.Name, lb.ipAddrID)
			return nil, fmt.Errorf("LB %s not managed by cloudprovider", lb.ipAddrID)
		}
	}

	glog.V(4).Infof("Load balancer %v is associated with IP %v", lb.name, lb.ipAddr)

	status = &v1.LoadBalancerStatus{}
	status.Ingress = []v1.LoadBalancerIngress{{IP: lb.ipAddr, Hostname: lb.name}}

	if lb.projectID != "" && service.Labels[cs.projectIDLabel] == "" {
		service.Labels[cs.projectIDLabel] = lb.projectID
	}

	// If the load balancer rule exists and is up-to-date, we move on to the next rule.
	exists, needsUpdate, err := lb.checkLoadBalancerRule(lb.name, service.Spec.Ports)
	if err != nil {
		return nil, err
	}
	if exists && !needsUpdate {
		glog.V(4).Infof("Load balancer rule %v is up-to-date", lb.name)
		return status, nil
	}

	if needsUpdate {
		glog.V(4).Infof("Updating load balancer rule: %v", lb.name)
		if errLB := lb.updateLoadBalancerRule(lb.name); errLB != nil {
			return nil, errLB
		}
		return status, nil
	}

	glog.V(4).Infof("Creating load balancer rule: %v", lb.name)
	lbRule, err := lb.createLoadBalancerRule(lb.name, service.Spec.Ports)
	if err != nil {
		return nil, err
	}

	defer func(rule *loadBalancerRule) {
		if err != nil {
			if deleteErr := lb.deleteLoadBalancerRule(rule); deleteErr != nil {
				glog.Errorf(deleteErr.Error())
			}
		}
	}(lbRule)

	glog.V(4).Infof("Assigning tag to load balancer rule: %v", lb.name)
	if err = lb.assignTagsToRule(lbRule, service); err != nil {
		return nil, err
	}

	glog.V(4).Infof("Assigning networks (%v) to load balancer rule: %v", lb.networkIDs, lb.name)
	if err = lb.assignNetworksToRule(lbRule, lb.networkIDs); err != nil {
		return nil, err
	}

	glog.V(4).Infof("Assigning hosts (%v) to load balancer rule: %v", lb.hostIDs, lb.name)
	if err = lb.assignHostsToRule(lbRule, lb.hostIDs); err != nil {
		return nil, err
	}

	return status, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (cs *CSCloud) UpdateLoadBalancer(clusterName string, service *v1.Service, nodes []*v1.Node) error {
	glog.V(5).Infof("UpdateLoadBalancer(%v, %v, %v, %#v)", clusterName, service.Namespace, service.Name, nodes)

	nodes = cs.filterNodesMatchingLabels(nodes, *service)

	hostIDs, networkIDs, projectID, err := cs.extractIDs(nodes)
	if err != nil {
		return err
	}

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, projectID)
	if err != nil {
		return err
	}
	lb.hostIDs = hostIDs
	lb.networkIDs = networkIDs

	manage, err := shouldManageLB(lb, service)
	if err != nil {
		return fmt.Errorf("failed to check if LB should be managed: %v", err)
	}
	if !manage {
		glog.V(3).Infof("Skipping UpdateLoadBalancer for service %s and LB %s", service.Name, lb.ipAddrID)
		return nil
	}

	client, err := lb.getClient()
	if err != nil {
		return err
	}

	if lb.rule == nil {
		return nil
	}

	p := client.LoadBalancer.NewListLoadBalancerRuleInstancesParams(lb.rule.Id)

	// Retrieve all VMs currently associated to this load balancer rule.
	l, err := client.LoadBalancer.ListLoadBalancerRuleInstances(p)
	if err != nil {
		return fmt.Errorf("error retrieving associated instances: %v", err)
	}

	assign, remove := symmetricDifference(lb.hostIDs, l.LoadBalancerRuleInstances)

	if len(assign) > 0 {

		glog.V(4).Infof("Assigning networks (%v) to load balancer rule: %v", lb.networkIDs, lb.rule.Name)
		if err := lb.assignNetworksToRule(lb.rule, lb.networkIDs); err != nil {
			return err
		}

		glog.V(4).Infof("Assigning new hosts (%v) to load balancer rule: %v", assign, lb.rule.Name)
		if err := lb.assignHostsToRule(lb.rule, assign); err != nil {
			return err
		}
	}

	if len(remove) > 0 {
		glog.V(4).Infof("Removing old hosts (%v) from load balancer rule: %v", assign, lb.rule.Name)
		if err := lb.removeHostsFromRule(lb.rule, remove); err != nil {
			return err
		}
	}

	return nil
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it exists, returning
// nil if the load balancer specified either didn't exist or was successfully deleted.
func (cs *CSCloud) EnsureLoadBalancerDeleted(clusterName string, service *v1.Service) error {
	glog.V(5).Infof("EnsureLoadBalancerDeleted(%v, %v, %v)", clusterName, service.Namespace, service.Name)

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service, "")
	if err != nil {
		return err
	}

	if !lb.environments[lb.environment].removeLBs {
		glog.V(3).Infof("skipping deletion of load balancer %s: environment has removals disabled.", lb.ipAddrID)
		return nil
	}

	manage, err := shouldManageLB(lb, service)
	if err != nil {
		return fmt.Errorf("failed to check if LB should be managed: %v", err)
	}
	if !manage {
		glog.V(3).Infof("Skipping EnsureLoadBalancerDeleted for service %s and LB %s", service.Name, lb.ipAddrID)
		return nil
	}

	glog.V(4).Infof("Deleting load balancer provider tag: %v", lb.ipAddrID)
	if err := lb.deleteTags(service); err != nil {
		return err
	}

	if lb.rule != nil {
		if err := lb.deleteLoadBalancerRule(lb.rule); err != nil {
			return err
		}
	}

	if lb.ipAddr != "" && lb.ipAddr != service.Spec.LoadBalancerIP {
		glog.V(4).Infof("Releasing load balancer IP: %v", lb.ipAddr)
		if err := lb.releaseLoadBalancerIP(); err != nil {
			return err
		}
	}

	return nil
}

// getLoadBalancer retrieves the IP address and ID and all the existing rules it can find.
func (cs *CSCloud) getLoadBalancer(service *v1.Service, projectID string) (*loadBalancer, error) {
	if projectID == "" {
		var ok bool
		projectID, ok = getLabelOrAnnotation(service.ObjectMeta, cs.projectIDLabel)
		if !ok {
			glog.V(4).Infof("unable to retrive projectID for service: %#v", service)
		}
	}
	environment, _ := getLabelOrAnnotation(service.ObjectMeta, cs.environmentLabel)
	lb := &loadBalancer{
		CSCloud:     cs,
		name:        cs.getLoadBalancerName(*service),
		projectID:   projectID,
		environment: environment,
	}

	client, err := lb.getClient()
	if err != nil {
		glog.V(4).Infof("unable to retrieve cloudstack client for load balancer %q for service %v/%v: %v", lb.name, service.Namespace, service.Name, err)
		return lb, nil
	}

	lb.rule, err = getLoadBalancerRule(client, lb.name, projectID)
	if err != nil {
		return nil, fmt.Errorf("load balancer %s for service %v/%v get rule error: %v", lb.name, service.Namespace, service.Name, err)
	}

	if lb.rule != nil {
		lb.ipAddr = lb.rule.Publicip
		lb.ipAddrID = lb.rule.Publicipid
	}

	return lb, nil
}

func getLoadBalancerRule(client *cloudstack.CloudStackClient, lbName, projectID string) (*loadBalancerRule, error) {
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
	if result.Count > 1 {
		return nil, fmt.Errorf("lb %q too many rules associated: %#v", lbName, result.LoadBalancerRules)
	}
	if result.Count == 0 {
		return nil, nil
	}

	return result.LoadBalancerRules[0], nil
}

// extractIDs extracts the VM ID for each node, their unique network IDs and project ID
func (cs *CSCloud) extractIDs(nodes []*v1.Node) ([]string, []string, string, error) {
	if len(nodes) == 0 {
		glog.V(4).Info("skipping extractIDs for empty node slice")
		return nil, nil, "", nil
	}
	hostNames := map[string]bool{}
	for _, node := range nodes {
		if name, ok := getLabelOrAnnotation(node.ObjectMeta, cs.nodeNameLabel); ok {
			hostNames[name] = true
			continue
		}
		hostNames[node.Name] = true
	}

	environmentID, _ := getLabelOrAnnotation(nodes[0].ObjectMeta, cs.environmentLabel)

	projectID, ok := getLabelOrAnnotation(nodes[0].ObjectMeta, cs.projectIDLabel)
	if !ok {
		return nil, nil, "", fmt.Errorf("unable to retrieve projectID for node %#v", nodes[0])
	}

	var client *cloudstack.CloudStackClient
	if env, ok := cs.environments[environmentID]; ok {
		client = env.client
	}
	if client == nil {
		return nil, nil, "", fmt.Errorf("unable to retrieve cloudstack client for environment %q", environmentID)
	}

	p := client.VirtualMachine.NewListVirtualMachinesParams()
	p.SetListall(true)

	if projectID != "" {
		p.SetProjectid(projectID)
	}

	l, err := client.VirtualMachine.ListVirtualMachines(p)
	if err != nil {
		return nil, nil, "", fmt.Errorf("error retrieving list of hosts: %v", err)
	}

	var hostIDs []string
	var networkIDs []string

	networkMap := make(map[string]struct{})
	// Check if the virtual machine is in the hosts slice, then add the corresponding IDs.
	for _, vm := range l.VirtualMachines {
		if !hostNames[vm.Name] {
			continue
		}
		hostIDs = append(hostIDs, vm.Id)

		if _, ok := networkMap[vm.Nic[0].Networkid]; ok {
			continue
		}
		networkMap[vm.Nic[0].Networkid] = struct{}{}
		networkIDs = append(networkIDs, vm.Nic[0].Networkid)
	}

	return hostIDs, networkIDs, projectID, nil
}

func (cs *CSCloud) filterNodesMatchingLabels(nodes []*v1.Node, service v1.Service) []*v1.Node {
	if cs.serviceLabel == "" || cs.nodeLabel == "" {
		return nodes
	}
	labelValue, _ := getLabelOrAnnotation(service.ObjectMeta, cs.serviceLabel)

	var filteredNodes []*v1.Node
	for i := range nodes {
		nodeLabelValue, _ := getLabelOrAnnotation(nodes[i].ObjectMeta, cs.nodeLabel)
		if nodeLabelValue != labelValue {
			continue
		}
		filteredNodes = append(filteredNodes, nodes[i])
	}
	return filteredNodes
}

// getLoadBalancerName returns the name of the load balancer responsible for the service
// by looking at the service label. If not set, it fallsback to the
// concatanation of the service name and the environment load balancer domain for the environent
func (cs *CSCloud) getLoadBalancerName(service v1.Service) string {
	name, ok := getLabelOrAnnotation(service.ObjectMeta, lbNameLabel)
	if ok {
		return name
	}
	environment, ok := getLabelOrAnnotation(service.ObjectMeta, cs.environmentLabel)
	if !ok {
		return cloudprovider.GetLoadBalancerName(&service)
	}
	var lbDomain string
	if env, ok := cs.environments[environment]; ok {
		lbDomain = env.lbDomain
	}
	return fmt.Sprintf("%s.%s", service.Name, lbDomain)
}

// hasLoadBalancerIP returns true if we have a load balancer address and ID.
func (lb *loadBalancer) hasLoadBalancerIP() bool {
	return lb.ipAddr != "" && lb.ipAddrID != ""
}

// getLoadBalancerIP retieves an existing IP or associates a new IP.
func (lb *loadBalancer) getLoadBalancerIP(loadBalancerIP string) error {
	if loadBalancerIP != "" {
		return lb.getPublicIPAddress(loadBalancerIP)
	}

	return lb.associatePublicIPAddress()
}

// getPublicIPAddressID retrieves the ID of the given IP, and sets the address and it's ID.
func (lb *loadBalancer) getPublicIPAddress(loadBalancerIP string) error {
	glog.V(4).Infof("Retrieve load balancer IP details: %v", loadBalancerIP)
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	p := client.Address.NewListPublicIpAddressesParams()
	p.SetIpaddress(loadBalancerIP)
	p.SetListall(true)

	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}

	l, err := client.Address.ListPublicIpAddresses(p)
	if err != nil {
		return fmt.Errorf("error retrieving IP address: %v", err)
	}

	if l.Count != 1 {
		return fmt.Errorf("could not find IP address %v", loadBalancerIP)
	}

	lb.ipAddr = l.PublicIpAddresses[0].Ipaddress
	lb.ipAddrID = l.PublicIpAddresses[0].Id

	return nil
}

// associatePublicIPAddress associates a new IP and sets the address and it's ID.
func (lb *loadBalancer) associatePublicIPAddress() error {
	glog.V(4).Infof("Allocate new IP for load balancer: %v", lb.name)
	// If a network belongs to a VPC, the IP address needs to be associated with
	// the VPC instead of with the network.
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	network, count, err := client.Network.GetNetworkByID(lb.networkIDs[0], cloudstack.WithProject(lb.projectID))
	if err != nil {
		if count == 0 {
			return fmt.Errorf("could not find network %v", lb.networkIDs[0])
		}
		return fmt.Errorf("error retrieving network: %v", err)
	}

	pc := &cloudstack.CustomServiceParams{}
	if network.Vpcid != "" {
		pc.SetParam("vpcid", network.Vpcid)
	} else {
		pc.SetParam("networkid", lb.networkIDs[0])
	}
	if lb.projectID != "" {
		pc.SetParam("projectid", lb.projectID)
	}
	if lb.getLBEnvironmentID() != "" {
		pc.SetParam("lbenvironmentid", lb.getLBEnvironmentID())
	}

	var result map[string]string
	associateCommand := lb.CSCloud.customAssociateIPCommand
	if associateCommand == "" {
		associateCommand = "associateIpAddress"
	}
	err = client.Custom.CustomRequest(associateCommand, pc, &result)
	if err != nil {
		return fmt.Errorf("error associate new IP address using endpoint %q: %v", associateCommand, err)
	}

	lb.ipAddr = result["ipaddress"]
	lb.ipAddrID = result["id"]
	if jobid, ok := result["jobid"]; ok {
		glog.V(4).Infof("Querying async job %s for load balancer %s", jobid, lb.ipAddrID)
		pa := &cloudstack.QueryAsyncJobResultParams{}
		pa.SetJobid(jobid)
		rawAsync, err := client.GetAsyncJobResult(jobid, int64(time.Minute))
		if err != nil {
			return err
		}
		glog.V(4).Infof("Result: %s", string(rawAsync))
		var asyncResult struct {
			IPAddress struct {
				IPAddress string `json:"ipaddress,omitempty"`
			} `json:"ipaddress,omitempty"`
		}
		err = json.Unmarshal(rawAsync, &asyncResult)
		if err != nil {
			return err
		}
		lb.ipAddr = asyncResult.IPAddress.IPAddress
	}
	glog.V(4).Infof("Allocated IP %s for load balancer %s with name %v", lb.ipAddr, lb.ipAddrID, lb.name)

	return nil
}

// releasePublicIPAddress releases an associated IP.
func (lb *loadBalancer) releaseLoadBalancerIP() error {
	glog.V(4).Infof("Release IP %s (%s) for load balancer: %v", lb.ipAddr, lb.ipAddrID, lb.name)
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	pc := &cloudstack.CustomServiceParams{}
	pc.SetParam("id", lb.ipAddrID)
	if lb.projectID != "" {
		pc.SetParam("projectid", lb.projectID)
	}

	disassociateCommand := lb.CSCloud.customDisassociateIPCommand
	if disassociateCommand == "" {
		disassociateCommand = "disassociateIpAddress"
	}
	err = client.Custom.CustomRequest(disassociateCommand, pc, new(interface{}))
	if err != nil {
		return fmt.Errorf("error disassociate IP address using endpoint %q: %v", disassociateCommand, err)
	}
	return nil
}

func sortPorts(ports []v1.ServicePort) {
	sort.Slice(ports, func(i, j int) bool {
		return ports[i].Port < ports[j].Port
	})
}

func comparePorts(ports []v1.ServicePort, rule *loadBalancerRule) bool {
	sortPorts(ports)
	var additionalPorts []string
	for _, p := range ports[1:] {
		additionalPorts = append(additionalPorts, fmt.Sprintf("%d:%d", p.Port, p.NodePort))
	}
	sort.Strings(additionalPorts)
	sort.Strings(rule.AdditionalPortMap)
	if len(rule.AdditionalPortMap) == 0 {
		rule.AdditionalPortMap = nil
	}
	return reflect.DeepEqual(additionalPorts, rule.AdditionalPortMap) &&
		rule.Privateport == strconv.Itoa(int(ports[0].NodePort)) &&
		rule.Publicport == strconv.Itoa(int(ports[0].Port))
}

// checkLoadBalancerRule checks if the rule already exists and if it does, if it can be updated. If
// it does exist but cannot be updated, it will delete the existing rule so it can be created again.
func (lb *loadBalancer) checkLoadBalancerRule(lbRuleName string, ports []v1.ServicePort) (bool, bool, error) {
	if lb.rule == nil {
		return false, false, nil
	}
	if len(ports) == 0 {
		return false, false, errors.New("invalid ports")
	}

	// Check if any of the values we cannot update (those that require a new load balancer rule) are changed.
	if lb.rule.Publicip == lb.ipAddr && comparePorts(ports, lb.rule) {
		return true, lb.rule.Algorithm != lb.algorithm, nil
	}

	glog.V(4).Infof("checkLoadBalancerRule found differences, will delete LB %s: rule: %#v, lb: %#v, ports: %#v", lb.name, lb.rule, lb, ports)

	// Delete the load balancer rule so we can create a new one using the new values.
	if err := lb.deleteLoadBalancerRule(lb.rule); err != nil {
		return false, false, err
	}

	return false, false, nil
}

// updateLoadBalancerRule updates a load balancer rule.
func (lb *loadBalancer) updateLoadBalancerRule(lbRuleName string) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}

	p := client.LoadBalancer.NewUpdateLoadBalancerRuleParams(lb.rule.Id)
	p.SetAlgorithm(lb.algorithm)

	_, err = client.LoadBalancer.UpdateLoadBalancerRule(p)
	return err
}

// createLoadBalancerRule creates a new load balancer rule and returns it's ID.
func (lb *loadBalancer) createLoadBalancerRule(lbRuleName string, ports []v1.ServicePort) (*loadBalancerRule, error) {
	client, err := lb.getClient()
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
	p.SetParam("publicport", int(ports[0].Port))
	p.SetParam("networkid", lb.networkIDs[0])
	p.SetParam("publicipid", lb.ipAddrID)

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
		additionalPorts = append(additionalPorts, fmt.Sprintf("%d:%d", p.Port, p.NodePort))
	}
	if len(additionalPorts) > 0 {
		p.SetParam("additionalportmap", strings.Join(additionalPorts, ","))
	}

	// Do not create corresponding firewall rule.
	p.SetParam("openfirewall", false)

	// Create a new load balancer rule.
	r := cloudstack.CreateLoadBalancerRuleResponse{}
	err = client.Custom.CustomRequest("createLoadBalancerRule", p, &r)
	if err != nil {
		return nil, fmt.Errorf("error creating load balancer rule %v: %v", lbRuleName, err)
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
		},
	}

	return lbRule, nil
}

// deleteLoadBalancerRule deletes a load balancer rule.
func (lb *loadBalancer) deleteLoadBalancerRule(lbRule *loadBalancerRule) error {
	glog.V(4).Infof("Deleting load balancer rule: %v", lbRule.Name)
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	p := client.LoadBalancer.NewDeleteLoadBalancerRuleParams(lbRule.Id)

	if _, err := client.LoadBalancer.DeleteLoadBalancerRule(p); err != nil {
		return fmt.Errorf("error deleting load balancer rule %v: %v", lbRule.Name, err)
	}
	lb.rule = nil
	return nil
}

// shouldManageLB checks if LB has the provider tag and the corresponding service tag
func shouldManageLB(lb *loadBalancer, service *v1.Service) (bool, error) {
	hasTags, err := lb.hasTag(cloudProviderTag, ProviderName)
	if err != nil {
		return false, fmt.Errorf("failed to check load balancer provider tag: %v", err)
	}

	if !hasTags {
		glog.V(3).Infof("should NOT manage LB %s. Rules missing cloudprovider tag %q.", lb.ipAddrID, ProviderName)
		return false, nil
	}

	hasTags, err = lb.hasTag(serviceTag, service.Name)
	if err != nil {
		return false, fmt.Errorf("failed to check load balancer service tag: %v", err)
	}

	if !hasTags {
		glog.V(3).Infof("should NOT manage LB %s. Rules missing service tag %q.", lb.ipAddrID, service.Name)
		return false, nil
	}

	return true, nil
}

func (lb *loadBalancer) hasTag(k, v string) (bool, error) {
	if lb.rule == nil {
		return false, nil
	}
	client, err := lb.getClient()
	if err != nil {
		return false, err
	}
	p := client.Resourcetags.NewListTagsParams()
	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}
	p.SetResourceid(lb.rule.Id)
	p.SetResourcetype("LoadBalancer")
	p.SetKey(k)
	p.SetValue(v)
	res, err := client.Resourcetags.ListTags(p)
	if err != nil {
		return false, err
	}
	return res.Count != 0, nil
}

func (lb *loadBalancer) deleteTags(service *v1.Service) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	if lb.rule == nil {
		return nil
	}
	p := client.Resourcetags.NewDeleteTagsParams([]string{lb.rule.Id}, "LoadBalancer")
	p.SetTags(map[string]string{cloudProviderTag: ProviderName, serviceTag: service.Name})
	_, err = client.Resourcetags.DeleteTags(p)
	return err
}

func (lb *loadBalancer) assignTagsToRule(lbRule *loadBalancerRule, service *v1.Service) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	tags := map[string]string{cloudProviderTag: ProviderName, serviceTag: service.Name}
	tp := client.Resourcetags.NewCreateTagsParams([]string{lbRule.Id}, "LoadBalancer", tags)
	_, err = client.Resourcetags.CreateTags(tp)
	if err != nil {
		return fmt.Errorf("error adding tags to load balancer %s: %v", lbRule.Id, err)
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
		return fmt.Errorf("error assigning hosts to load balancer rule %v: %v", lbRule.Name, err)
	}

	return nil
}

// assignNetworksToRule assigns networks to a load balancer rule.
func (lb *loadBalancer) assignNetworksToRule(lbRule *loadBalancerRule, networkIDs []string) error {
	if lb.customAssignNetworksCommand == "" {
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
	glog.V(4).Infof("assign network %q to rule %s", networkID, lbRule.Id)
	p := &cloudstack.CustomServiceParams{}
	if lb.projectID != "" {
		p.SetParam("projectid", lb.projectID)
	}
	p.SetParam("id", lbRule.Id)
	p.SetParam("networkids", []string{networkID})
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	var result map[string]string
	if err := client.Custom.CustomRequest(lb.customAssignNetworksCommand, p, &result); err != nil {
		return fmt.Errorf("error assigning networks to load balancer rule %s using endpoint %q: %v ", lbRule.Id, lb.customAssignNetworksCommand, err)
	}
	if jobid, ok := result["jobid"]; ok {
		glog.V(4).Infof("Querying async job %s for load balancer rule %s", jobid, lbRule.Id)
		pa := &cloudstack.QueryAsyncJobResultParams{}
		pa.SetJobid(jobid)
		_, err := client.GetAsyncJobResult(jobid, int64(time.Minute))
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
	client := lb.CSCloud.environments[lb.environment].client
	if client == nil {
		return nil, fmt.Errorf("unable to retrive cloudstack client for lb: %#v", lb)
	}
	return client, nil
}

func (lb *loadBalancer) getLBEnvironmentID() string {
	return lb.CSCloud.environments[lb.environment].lbEnvironmentID
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
