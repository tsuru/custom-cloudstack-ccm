package cloudstack

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog"
)

// GetZone returns the Zone containing the current failure zone and locality region that the program is running in
// In most cases, this method is called from the kubelet querying a local metadata service to acquire its zone.
// For the case of external cloud providers, use GetZoneByProviderID or GetZoneByNodeName since GetZone
// can no longer be called from the kubelets.
func (cs *CSCloud) GetZone(ctx context.Context) (cloudprovider.Zone, error) {
	return cloudprovider.Zone{}, nil
}

// GetZoneByProviderID returns the Zone containing the current zone and locality region of the node specified by providerId
// This method is particularly used in the context of external cloud providers where node initialization must be done
// outside the kubelets.
func (cs *CSCloud) GetZoneByProviderID(ctx context.Context, providerID string) (cloudprovider.Zone, error) {
	klog.V(4).Infof("GetZoneByProviderID(%v)", providerID)
	zone := cloudprovider.Zone{}
	if providerID == "" {
		return zone, fmt.Errorf("empty providerID")
	}
	instance, err := cs.instanceByProviderID(providerID)
	if err != nil {
		return zone, err
	}
	klog.V(2).Infof("Zone for providerID %v is %v", providerID, instance.Zonename)
	zone.FailureDomain = instance.Zonename
	zone.Region = instance.Zonename
	return zone, nil
}

// GetZoneByNodeName returns the Zone containing the current zone and locality region of the node specified by node name
// This method is particularly used in the context of external cloud providers where node initialization must be done
// outside the kubelets.
func (cs *CSCloud) GetZoneByNodeName(ctx context.Context, nodeName types.NodeName) (cloudprovider.Zone, error) {
	klog.V(4).Infof("GetZoneByNodeName(%v)", nodeName)
	zone := cloudprovider.Zone{}
	if nodeName == "" {
		return zone, fmt.Errorf("empty providerID")
	}
	node, err := cs.getNodeByName(string(nodeName))
	if err != nil {
		return zone, fmt.Errorf("error retrieving node by name %q: %v", nodeName, err)
	}
	instance, err := cs.getInstanceForNode(node)
	if err != nil {
		if err == cloudprovider.InstanceNotFound {
			return zone, err
		}
		return zone, fmt.Errorf("error retrieving zone: %v", err)
	}
	klog.V(2).Infof("Zone for nodeName %v is %v", nodeName, instance.Zonename)
	zone.FailureDomain = instance.Zonename
	zone.Region = instance.Zonename
	return zone, nil
}
