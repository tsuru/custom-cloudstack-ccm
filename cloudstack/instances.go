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
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/xanzy/go-cloudstack/v2/cloudstack"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog"
)

type node struct {
	projectID   string
	name        string
	environment string
	vmID        string
}

func (n node) toProviderID() string {
	return strings.Join([]string{n.environment, n.projectID, n.vmID}, "/")
}

// NodeAddresses returns the addresses of the specified instance.
func (cs *CSCloud) NodeAddresses(ctx context.Context, name types.NodeName) ([]v1.NodeAddress, error) {
	klog.V(4).Infof("NodeAddresses(%v)", name)
	node, err := cs.getNodeByName(string(name))
	if err != nil {
		return nil, fmt.Errorf("error retrieving node by name %q: %v", string(name), err)
	}
	instance, err := cs.getInstanceForNode(node)
	if err != nil {
		if err == cloudprovider.InstanceNotFound {
			return nil, err
		}
		return nil, fmt.Errorf("error retrieving node addresses: %v", err)
	}
	return cs.nodeAddresses(instance)
}

// NodeAddressesByProviderID returns the addresses of the specified instance.
func (cs *CSCloud) NodeAddressesByProviderID(ctx context.Context, providerID string) ([]v1.NodeAddress, error) {
	klog.V(4).Infof("NodeAddressesByProviderID(%v)", providerID)
	if providerID == "" {
		return nil, fmt.Errorf("empty providerID")
	}
	instance, err := cs.instanceByProviderID(providerID)
	if err != nil {
		return nil, err
	}
	return cs.nodeAddresses(instance)
}

func (cs *CSCloud) nodeAddresses(instance *cloudstack.VirtualMachine) ([]v1.NodeAddress, error) {
	if len(instance.Nic) == 0 {
		return nil, errors.New("instance does not have an internal IP")
	}

	var addresses []v1.NodeAddress

	var internalAddr string
	internalIndex := cs.config.Global.InternalIPIndex
	if internalIndex < 0 || internalIndex >= len(instance.Nic) {
		klog.V(4).Infof("Unable to use index %v for internal IP, only %v NICs available, falling back to index 0", internalIndex, len(instance.Nic))
		internalIndex = 0
	}
	internalAddr = instance.Nic[internalIndex].Ipaddress
	addresses = append(addresses, v1.NodeAddress{Type: v1.NodeInternalIP, Address: internalAddr})

	if instance.Hostname != "" {
		addresses = append(addresses, v1.NodeAddress{Type: v1.NodeHostName, Address: instance.Hostname})
	} else {
		names := lookupAddrWithTimeout(internalAddr)
		for _, name := range names {
			// There's a chance for a PTR record to exist even when the A
			// record does not exist or is invalid. This resolves the name
			// returned in the PTR record to check if it reeeeaaallllly matches
			// the internal IP.
			ips := lookupHostWithTimeout(name)
			for _, ip := range ips {
				if ip == internalAddr {
					addresses = append(addresses, v1.NodeAddress{Type: v1.NodeHostName, Address: name})
					break
				}
			}
		}
	}

	externalIndex := cs.config.Global.ExternalIPIndex
	if externalIndex != internalIndex && externalIndex >= 0 && externalIndex < len(instance.Nic) {
		addresses = append(addresses, v1.NodeAddress{Type: v1.NodeExternalIP, Address: instance.Nic[externalIndex].Ipaddress})
	} else {
		klog.V(4).Infof("Unable to use index %v for external IP, only %v NICs available and %v is internal IP, ignoring", externalIndex, len(instance.Nic), internalIndex)
	}

	if instance.Publicip != "" {
		addresses = append(addresses, v1.NodeAddress{Type: v1.NodeExternalIP, Address: instance.Publicip})
	} else {
		// Since there is no sane way to determine the external IP if the host isn't
		// using static NAT, we will just fire a log message and omit the external IP.
		klog.V(4).Infof("Could not determine the public IP of host %v (%v)", instance.Name, instance.Id)
	}

	return addresses, nil
}

// InstanceID returns the cloud provider ID of the specified instance.
func (cs *CSCloud) InstanceID(ctx context.Context, name types.NodeName) (string, error) {
	klog.V(4).Infof("InstanceID(%v)", name)
	node, err := cs.getNodeByName(string(name))
	if err != nil {
		return "", fmt.Errorf("error retrieving node by name %q: %v", string(name), err)
	}
	instance, err := cs.getInstanceForNode(node)
	if err != nil {
		if err == cloudprovider.InstanceNotFound {
			return "", err
		}
		return "", fmt.Errorf("error retrieving instance ID: %v", err)
	}
	node.vmID = instance.Id
	return node.toProviderID(), nil
}

func parseProviderID(instanceID string) (node, error) {
	id := strings.TrimPrefix(instanceID, ProviderName+"://")
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return node{}, fmt.Errorf("error parsing providerID: %q - %#v", instanceID, parts)
	}
	return node{
		environment: parts[0],
		projectID:   parts[1],
		vmID:        parts[2],
	}, nil
}

// InstanceType returns the type of the specified instance.
func (cs *CSCloud) InstanceType(ctx context.Context, name types.NodeName) (string, error) {
	klog.V(4).Infof("InstanceType(%v)", name)
	node, err := cs.getNodeByName(string(name))
	if err != nil {
		return "", fmt.Errorf("error retrieving node by name %q: %v", string(name), err)
	}
	instance, err := cs.getInstanceForNode(node)
	if err != nil {
		if err == cloudprovider.InstanceNotFound {
			return "", err
		}
		return "", fmt.Errorf("error retrieving instance type: %v", err)
	}
	return instance.Serviceofferingid, nil
}

func (cs *CSCloud) instanceByProviderID(providerID string) (*cloudstack.VirtualMachine, error) {
	nodeIDData, err := parseProviderID(providerID)
	if err != nil {
		return nil, err
	}
	client, err := cs.clientForEnvironment(nodeIDData.environment)
	if err != nil {
		return nil, err
	}
	instance, count, err := client.VirtualMachine.GetVirtualMachineByID(
		nodeIDData.vmID,
		cloudstack.WithProject(nodeIDData.projectID),
	)
	if count == 0 {
		return nil, cloudprovider.InstanceNotFound
	}
	if err != nil {
		err = fmt.Errorf("error retrieving instance: %v", err)
	}
	return instance, err
}

// InstanceTypeByProviderID returns the type of the specified instance.
func (cs *CSCloud) InstanceTypeByProviderID(ctx context.Context, providerID string) (string, error) {
	klog.V(4).Infof("InstanceTypeByProviderID(%v)", providerID)
	if providerID == "" {
		return "", fmt.Errorf("empty providerID")
	}
	instance, err := cs.instanceByProviderID(providerID)
	if err != nil {
		return "", err
	}
	return instance.Serviceofferingname, nil
}

// AddSSHKeyToAllInstances is currently not implemented.
func (cs *CSCloud) AddSSHKeyToAllInstances(ctx context.Context, user string, keyData []byte) error {
	klog.V(4).Infof("AddSSHKeyToAllInstances(%v, %v)", user, keyData)
	return errors.New("AddSSHKeyToAllInstances not implemented")
}

// CurrentNodeName returns the name of the node we are currently running on.
func (cs *CSCloud) CurrentNodeName(ctx context.Context, hostname string) (types.NodeName, error) {
	klog.V(4).Infof("CurrentNodeName(%v)", hostname)
	return types.NodeName(hostname), nil
}

// InstanceExistsByProviderID returns if the instance still exists.
func (cs *CSCloud) InstanceExistsByProviderID(ctx context.Context, providerID string) (bool, error) {
	klog.V(4).Infof("InstanceExistsByProviderID(%v)", providerID)
	if providerID == "" {
		return false, fmt.Errorf("empty providerID")
	}
	_, err := cs.instanceByProviderID(providerID)
	if err != nil {
		if err == cloudprovider.InstanceNotFound {
			err = nil
		}
		return false, err
	}
	return true, nil
}

// InstanceShutdownByProviderID returns true if the instance is shutdown in cloudprovider
func (cs *CSCloud) InstanceShutdownByProviderID(ctx context.Context, providerID string) (bool, error) {
	klog.V(4).Infof("InstanceShutdownByProviderID(%v)", providerID)
	if providerID == "" {
		return false, fmt.Errorf("empty providerID")
	}
	instance, err := cs.instanceByProviderID(providerID)
	if err != nil {
		return false, err
	}
	// States from https://github.com/apache/cloudstack/blob/87c43501608a1df72a2f01ed17a522233e6617b0/api/src/main/java/com/cloud/vm/VirtualMachine.java#L45
	switch instance.State {
	case "Stopping", "Stopped", "Destroyed", "Expunging", "Shutdowned", "Error":
		return true, nil
	case "Unknown":
		return false, errors.New("unknown vm state in cloudstack")
	}
	return false, nil
}

func (cs *CSCloud) getInstanceForNode(n *node) (*cloudstack.VirtualMachine, error) {
	client, err := cs.clientForNode(n)
	if err != nil {
		return nil, err
	}
	instance, count, err := client.VirtualMachine.GetVirtualMachineByName(
		n.name,
		cloudstack.WithProject(n.projectID),
	)
	if err != nil {
		klog.V(4).Infof("getInstanceForNode(%v): Could not find VM: %v", n, err)
		if count == 0 {
			return nil, cloudprovider.InstanceNotFound
		}
		return nil, fmt.Errorf("error instance for node %v: %v", n, err)
	}
	klog.V(4).Infof("getInstanceForNode(%v): Found instance %#v", n, instance)
	return instance, nil
}

func (cs *CSCloud) getNodeByName(name string) (*node, error) {
	kubeNode, err := cs.kubeClient.CoreV1().Nodes().Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	n, err := cs.newNode(kubeNode)
	if err != nil {
		return nil, err
	}
	klog.V(4).Infof("getNodeByName(%v): found node: %#v", name, n)
	return n, nil
}

func (cs *CSCloud) newNode(kubeNode *v1.Node) (*node, error) {
	name := kubeNode.Name
	if n, ok := getLabelOrAnnotation(kubeNode.ObjectMeta, cs.config.Global.NodeNameLabel); ok {
		name = n
	}
	environment, _ := getLabelOrAnnotation(kubeNode.ObjectMeta, cs.config.Global.EnvironmentLabel)
	projectID, ok := getLabelOrAnnotation(kubeNode.ObjectMeta, cs.config.Global.ProjectIDLabel)
	if !ok {
		return nil, fmt.Errorf("failed to retrieve projectID from node %#v", kubeNode)
	}
	n := &node{
		projectID:   projectID,
		environment: environment,
		name:        name,
		vmID:        kubeNode.Spec.ProviderID,
	}
	return n, nil
}

func (cs *CSCloud) clientForNode(n *node) (*cloudstack.CloudStackClient, error) {
	return cs.clientForEnvironment(n.environment)
}

func (cs *CSCloud) clientForEnvironment(environment string) (*cloudstack.CloudStackClient, error) {
	client := cs.environments[environment].client
	if client == nil {
		return nil, fmt.Errorf("unable to retrieve client for environment %v, available environments: %#v", environment, cs.environments)
	}
	return client, nil
}

func getLabelOrAnnotation(obj metav1.ObjectMeta, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if n, ok := obj.Labels[name]; ok {
		return n, true
	}
	n, ok := obj.Annotations[name]
	return n, ok
}

func lookupAddrWithTimeout(addr string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	names, _ := net.DefaultResolver.LookupAddr(ctx, addr)
	return names
}

func lookupHostWithTimeout(host string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, _ := net.DefaultResolver.LookupHost(ctx, host)
	return ips
}
