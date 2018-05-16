package cloudstack

import (
	"fmt"

	"github.com/golang/glog"
	"github.com/xanzy/go-cloudstack/cloudstack"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/cloudprovider"
)

// GetZone returns the Zone containing the current failure zone and locality region that the program is running in
// In most cases, this method is called from the kubelet querying a local metadata service to aquire its zone.
// For the case of external cloud providers, use GetZoneByProviderID or GetZoneByNodeName since GetZone
// can no longer be called from the kubelets.
func (cs *CSCloud) GetZone() (cloudprovider.Zone, error) {
	return cloudprovider.Zone{}, nil
}

// GetZoneByProviderID returns the Zone containing the current zone and locality region of the node specified by providerId
// This method is particularly used in the context of external cloud providers where node initialization must be done
// outside the kubelets.
func (cs *CSCloud) GetZoneByProviderID(providerID string) (cloudprovider.Zone, error) {
	glog.V(4).Infof("GetZoneByProviderID(%v)", providerID)
	zone := cloudprovider.Zone{}
	if providerID == "" {
		return zone, fmt.Errorf("empty providerID")
	}
	node, err := cs.getNodeByProviderID(providerID)
	if err != nil {
		return zone, fmt.Errorf("error retrieving node by providerID %q: %v", providerID, err)
	}
	client, err := cs.clientForNode(node)
	if err != nil {
		return zone, err
	}
	instance, count, err := client.VirtualMachine.GetVirtualMachineByID(
		providerID,
		cloudstack.WithProject(node.projectID),
	)
	if err != nil {
		if count == 0 {
			return zone, fmt.Errorf("could not find node by ID: %v", providerID)
		}
		return zone, fmt.Errorf("error retrieving zone: %v", err)
	}
	glog.V(2).Infof("Zone for providerID %v is %v", providerID, instance.Zonename)
	zone.FailureDomain = instance.Zonename
	zone.Region = instance.Zonename
	return zone, nil
}

// GetZoneByNodeName returns the Zone containing the current zone and locality region of the node specified by node name
// This method is particularly used in the context of external cloud providers where node initialization must be done
// outside the kubelets.
func (cs *CSCloud) GetZoneByNodeName(nodeName types.NodeName) (cloudprovider.Zone, error) {
	glog.V(4).Infof("GetZoneByNodeName(%v)", nodeName)
	zone := cloudprovider.Zone{}
	if nodeName == "" {
		return zone, fmt.Errorf("empty providerID")
	}
	node, err := cs.getNodeByName(string(nodeName))
	if err != nil {
		return zone, fmt.Errorf("error retrieving node by name %q: %v", nodeName, err)
	}
	client, err := cs.clientForNode(node)
	if err != nil {
		return zone, err
	}
	instance, count, err := client.VirtualMachine.GetVirtualMachineByName(
		string(nodeName),
		cloudstack.WithProject(node.projectID),
	)
	if err != nil {
		if count == 0 {
			return zone, fmt.Errorf("could not find node by name: %v", nodeName)
		}
		return zone, fmt.Errorf("error retrieving zone: %v", err)
	}
	glog.V(2).Infof("Zone for nodeName %v is %v", nodeName, instance.Zonename)
	zone.FailureDomain = instance.Zonename
	zone.Region = instance.Zonename
	return zone, nil
}
