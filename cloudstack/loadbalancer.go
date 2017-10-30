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
	"fmt"
	"strconv"
	"time"

	"github.com/golang/glog"
	"github.com/xanzy/go-cloudstack/cloudstack"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/cloudprovider"
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
	rules       map[string]*cloudstack.LoadBalancerRule
}

// GetLoadBalancer returns whether the specified load balancer exists, and if so, what its status is.
func (cs *CSCloud) GetLoadBalancer(clusterName string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	glog.V(4).Infof("GetLoadBalancer(%v, %v, %v)", clusterName, service.Namespace, service.Name)

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service)
	if err != nil {
		return nil, false, err
	}

	// If we don't have any rules, the load balancer does not exist.
	if len(lb.rules) == 0 {
		return nil, false, nil
	}

	glog.V(4).Infof("Found a load balancer associated with IP %v", lb.ipAddr)

	status := &v1.LoadBalancerStatus{}
	status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: lb.ipAddr})

	return status, true, nil
}

// EnsureLoadBalancer creates a new load balancer, or updates the existing one. Returns the status of the balancer.
func (cs *CSCloud) EnsureLoadBalancer(clusterName string, service *v1.Service, nodes []*v1.Node) (status *v1.LoadBalancerStatus, err error) {
	glog.V(4).Infof("EnsureLoadBalancer(%v, %v, %v, %v, %v, %v)", clusterName, service.Namespace, service.Name, service.Spec.LoadBalancerIP, service.Spec.Ports, nodes)

	if len(service.Spec.Ports) == 0 {
		return nil, fmt.Errorf("requested load balancer with no ports")
	}

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service)
	if err != nil {
		return nil, err
	}

	lb.setAlgorithm(service)

	nodes = cs.filterNodesMatchingLabels(nodes, *service)

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes available to add to load balancer")
	}

	lb.hostIDs, lb.networkIDs, lb.projectID, err = cs.extractIDs(nodes)
	if err != nil {
		return nil, err
	}

	glog.V(4).Infof("Ensuring Load Balancer: %+v", lb)

	if !lb.hasLoadBalancerIP() {
		// Create or retrieve the load balancer IP.
		if errLB := lb.getLoadBalancerIP(service.Spec.LoadBalancerIP); errLB != nil {
			return nil, errLB
		}

		if lb.ipAddr != "" && lb.ipAddr != service.Spec.LoadBalancerIP {
			defer func(lb *loadBalancer) {
				if err != nil {
					if err := lb.releaseLoadBalancerIP(); err != nil {
						glog.Errorf(err.Error())
					}
				}
			}(lb)
		}
	}

	glog.V(4).Infof("Load balancer %v is associated with IP %v", lb.name, lb.ipAddr)

	for _, port := range service.Spec.Ports {
		lbRuleName := lb.name

		// If the load balancer rule exists and is up-to-date, we move on to the next rule.
		exists, needsUpdate, err := lb.checkLoadBalancerRule(lbRuleName, port)
		if err != nil {
			return nil, err
		}
		if exists && !needsUpdate {
			glog.V(4).Infof("Load balancer rule %v is up-to-date", lbRuleName)
			// Delete the rule from the map, to prevent it being deleted.
			delete(lb.rules, lbRuleName)
			continue
		}

		if needsUpdate {
			glog.V(4).Infof("Updating load balancer rule: %v", lbRuleName)
			if errLB := lb.updateLoadBalancerRule(lbRuleName); errLB != nil {
				return nil, errLB
			}
			// Delete the rule from the map, to prevent it being deleted.
			delete(lb.rules, lbRuleName)
			continue
		}

		glog.V(4).Infof("Creating load balancer rule: %v", lbRuleName)
		lbRule, err := lb.createLoadBalancerRule(lbRuleName, port)
		if err != nil {
			return nil, err
		}

		glog.V(4).Infof("Assigning hosts (%v) to load balancer rule: %v", lb.hostIDs, lbRuleName)
		if err = lb.assignHostsToRule(lbRule, lb.hostIDs); err != nil {
			return nil, err
		}

		glog.V(4).Infof("Assigning networks (%v) to load balancer rule: %v", lb.networkIDs, lbRuleName)
		if err = lb.assignNetworksToRule(lbRule, lb.networkIDs); err != nil {
			return nil, err
		}

	}

	// Cleanup any rules that are now still in the rules map, as they are no longer needed.
	for _, lbRule := range lb.rules {
		glog.V(4).Infof("Deleting obsolete load balancer rule: %v", lbRule.Name)
		if err := lb.deleteLoadBalancerRule(lbRule); err != nil {
			return nil, err
		}
	}

	status = &v1.LoadBalancerStatus{}
	status.Ingress = []v1.LoadBalancerIngress{{IP: lb.ipAddr}}

	if lb.projectID != "" && service.Labels[cs.projectIDLabel] == "" {
		service.Labels[cs.projectIDLabel] = lb.projectID
	}
	return status, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (cs *CSCloud) UpdateLoadBalancer(clusterName string, service *v1.Service, nodes []*v1.Node) error {
	glog.V(4).Infof("UpdateLoadBalancer(%v, %v, %v, %v)", clusterName, service.Namespace, service.Name, nodes)

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service)
	if err != nil {
		return err
	}

	nodes = cs.filterNodesMatchingLabels(nodes, *service)

	lb.hostIDs, lb.networkIDs, lb.projectID, err = cs.extractIDs(nodes)
	if err != nil {
		return err
	}

	client, err := lb.getClient()
	if err != nil {
		return err
	}
	for _, lbRule := range lb.rules {
		p := client.LoadBalancer.NewListLoadBalancerRuleInstancesParams(lbRule.Id)

		// Retrieve all VMs currently associated to this load balancer rule.
		l, err := client.LoadBalancer.ListLoadBalancerRuleInstances(p)
		if err != nil {
			return fmt.Errorf("error retrieving associated instances: %v", err)
		}

		assign, remove := symmetricDifference(lb.hostIDs, l.LoadBalancerRuleInstances)

		if len(assign) > 0 {
			glog.V(4).Infof("Assigning new hosts (%v) to load balancer rule: %v", assign, lbRule.Name)
			if err := lb.assignHostsToRule(lbRule, assign); err != nil {
				return err
			}

			glog.V(4).Infof("Assigning networks (%v) to load balancer rule: %v", lb.networkIDs, lbRule.Name)
			if err := lb.assignNetworksToRule(lbRule, lb.networkIDs); err != nil {
				return err
			}
		}

		if len(remove) > 0 {
			glog.V(4).Infof("Removing old hosts (%v) from load balancer rule: %v", assign, lbRule.Name)
			if err := lb.removeHostsFromRule(lbRule, remove); err != nil {
				return err
			}
		}
	}

	return nil
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it exists, returning
// nil if the load balancer specified either didn't exist or was successfully deleted.
func (cs *CSCloud) EnsureLoadBalancerDeleted(clusterName string, service *v1.Service) error {
	glog.V(4).Infof("EnsureLoadBalancerDeleted(%v, %v, %v)", clusterName, service.Namespace, service.Name)

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(service)
	if err != nil {
		return err
	}

	for _, lbRule := range lb.rules {
		glog.V(4).Infof("Deleting load balancer rule: %v", lbRule.Name)
		if err := lb.deleteLoadBalancerRule(lbRule); err != nil {
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
func (cs *CSCloud) getLoadBalancer(service *v1.Service) (*loadBalancer, error) {
	projectID, ok := getLabelOrAnnotation(service.ObjectMeta, cs.projectIDLabel)
	if !ok {
		return nil, fmt.Errorf("unable to retrive projectID for service: %v", service)
	}
	environment, _ := getLabelOrAnnotation(service.ObjectMeta, cs.environmentLabel)
	lb := &loadBalancer{
		CSCloud:     cs,
		name:        cs.getLoadBalancerName(*service),
		projectID:   projectID,
		environment: environment,
		rules:       make(map[string]*cloudstack.LoadBalancerRule),
	}

	client, err := lb.getClient()
	if err != nil {
		return nil, err
	}

	p := client.LoadBalancer.NewListLoadBalancerRulesParams()
	p.SetKeyword(lb.name)
	p.SetListall(true)

	if projectID != "" {
		p.SetProjectid(projectID)
	}

	l, err := client.LoadBalancer.ListLoadBalancerRules(p)
	if err != nil {
		return nil, fmt.Errorf("error retrieving load balancer rules: %v", err)
	}

	for _, lbRule := range l.LoadBalancerRules {
		lb.rules[lbRule.Name] = lbRule

		if lb.ipAddr != "" && lb.ipAddr != lbRule.Publicip {
			glog.Warningf("Load balancer for service %v/%v has rules associated with different IP's: %v, %v", service.Namespace, service.Name, lb.ipAddr, lbRule.Publicip)
		}

		lb.ipAddr = lbRule.Publicip
		lb.ipAddrID = lbRule.Publicipid
	}

	glog.V(4).Infof("Load balancer %v contains %d rule(s)", lb.name, len(lb.rules))

	return lb, nil
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
		return nil, nil, "", fmt.Errorf("unable to retrieve projectID for node %v", nodes[0])
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
	labelValue := service.Labels[cs.serviceLabel]

	var filteredNodes []*v1.Node
	for i := range nodes {
		if nodes[i].Labels[cs.nodeLabel] != labelValue {
			continue
		}
		filteredNodes = append(filteredNodes, nodes[i])
	}
	return filteredNodes
}

func (cs *CSCloud) getLoadBalancerName(service v1.Service) string {
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
		glog.V(4).Infof("Querying async job %s for load balancer %s ", jobid, lb.ipAddrID)
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
	var data map[string]interface{}
	err = client.Custom.CustomRequest(disassociateCommand, pc, &data)
	if err != nil {
		return fmt.Errorf("error disassociate IP address using endpoint %q: %v", disassociateCommand, err)
	}
	return nil
}

// checkLoadBalancerRule checks if the rule already exists and if it does, if it can be updated. If
// it does exist but cannot be updated, it will delete the existing rule so it can be created again.
func (lb *loadBalancer) checkLoadBalancerRule(lbRuleName string, port v1.ServicePort) (bool, bool, error) {
	lbRule, ok := lb.rules[lbRuleName]
	if !ok {
		return false, false, nil
	}

	// Check if any of the values we cannot update (those that require a new load balancer rule) are changed.
	if lbRule.Publicip == lb.ipAddr && lbRule.Privateport == strconv.Itoa(int(port.NodePort)) && lbRule.Publicport == strconv.Itoa(int(port.Port)) {
		return true, lbRule.Algorithm != lb.algorithm, nil
	}

	// Delete the load balancer rule so we can create a new one using the new values.
	if err := lb.deleteLoadBalancerRule(lbRule); err != nil {
		return false, false, err
	}

	return false, false, nil
}

// updateLoadBalancerRule updates a load balancer rule.
func (lb *loadBalancer) updateLoadBalancerRule(lbRuleName string) error {
	lbRule := lb.rules[lbRuleName]
	client, err := lb.getClient()
	if err != nil {
		return err
	}

	p := client.LoadBalancer.NewUpdateLoadBalancerRuleParams(lbRule.Id)
	p.SetAlgorithm(lb.algorithm)

	_, err = client.LoadBalancer.UpdateLoadBalancerRule(p)
	return err
}

// createLoadBalancerRule creates a new load balancer rule and returns it's ID.
func (lb *loadBalancer) createLoadBalancerRule(lbRuleName string, port v1.ServicePort) (*cloudstack.LoadBalancerRule, error) {
	client, err := lb.getClient()
	if err != nil {
		return nil, err
	}
	p := client.LoadBalancer.NewCreateLoadBalancerRuleParams(
		lb.algorithm,
		lbRuleName,
		int(port.NodePort),
		int(port.Port),
	)

	p.SetNetworkid(lb.networkIDs[0])
	p.SetPublicipid(lb.ipAddrID)

	switch port.Protocol {
	case v1.ProtocolTCP:
		p.SetProtocol("TCP")
	case v1.ProtocolUDP:
		p.SetProtocol("UDP")
	default:
		return nil, fmt.Errorf("unsupported load balancer protocol: %v", port.Protocol)
	}

	// Do not create corresponding firewall rule.
	p.SetOpenfirewall(false)

	// Create a new load balancer rule.
	r, err := client.LoadBalancer.CreateLoadBalancerRule(p)
	if err != nil {
		return nil, fmt.Errorf("error creating load balancer rule %v: %v", lbRuleName, err)
	}

	lbRule := &cloudstack.LoadBalancerRule{
		Id:          r.Id,
		Algorithm:   r.Algorithm,
		Cidrlist:    r.Cidrlist,
		Name:        r.Name,
		Networkid:   r.Networkid,
		Privateport: r.Privateport,
		Publicport:  r.Publicport,
		Publicip:    r.Publicip,
		Publicipid:  r.Publicipid,
	}

	return lbRule, nil
}

// deleteLoadBalancerRule deletes a load balancer rule.
func (lb *loadBalancer) deleteLoadBalancerRule(lbRule *cloudstack.LoadBalancerRule) error {
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	p := client.LoadBalancer.NewDeleteLoadBalancerRuleParams(lbRule.Id)

	if _, err := client.LoadBalancer.DeleteLoadBalancerRule(p); err != nil {
		return fmt.Errorf("error deleting load balancer rule %v: %v", lbRule.Name, err)
	}

	// Delete the rule from the map as it no longer exists
	delete(lb.rules, lbRule.Name)

	return nil
}

// assignHostsToRule assigns hosts to a load balancer rule.
func (lb *loadBalancer) assignHostsToRule(lbRule *cloudstack.LoadBalancerRule, hostIDs []string) error {
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
func (lb *loadBalancer) assignNetworksToRule(lbRule *cloudstack.LoadBalancerRule, networkIDs []string) error {
	if lb.customAssignNetworksCommand == "" {
		return nil
	}
	p := &cloudstack.CustomServiceParams{}
	if lb.projectID != "" {
		p.SetParam("projectid", lb.projectID)
	}
	p.SetParam("id", lbRule.Id)
	p.SetParam("networkids", networkIDs)
	client, err := lb.getClient()
	if err != nil {
		return err
	}
	if err := client.Custom.CustomRequest(lb.customAssignNetworksCommand, p, nil); err != nil {
		return fmt.Errorf("error assigning networks to load balancer rule %s using endpoint %q: %v ", lbRule.Id, lb.customAssignNetworksCommand, err)
	}
	return nil
}

// removeHostsFromRule removes hosts from a load balancer rule.
func (lb *loadBalancer) removeHostsFromRule(lbRule *cloudstack.LoadBalancerRule, hostIDs []string) error {
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
		return nil, fmt.Errorf("unable to retrive cloudstack client for lb: %v", lb)
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
