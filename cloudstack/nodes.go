package cloudstack

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
)

type nodeInfo struct {
	name          string
	vmID          string
	networkID     string
	hostName      string
	projectID     string
	environmentID string
	filterValue   string
	revision      uint64
}

type nodeRegistry struct {
	nodes      map[string]*nodeInfo
	nodesMu    sync.RWMutex
	svcNodes   map[serviceKey]sets.String
	svcNodesMu sync.RWMutex
	cs         *CSCloud
	revision   uint64
}

func newNodeRegistry(cs *CSCloud) *nodeRegistry {
	return &nodeRegistry{
		nodes:    map[string]*nodeInfo{},
		svcNodes: map[serviceKey]sets.String{},
		cs:       cs,
		revision: 0,
	}
}

func nodeInfoNames(nodes []nodeInfo) string {
	names := make([]string, len(nodes))
	for i := range nodes {
		names[i] = nodes[i].name
	}
	return strings.Join(names, ",")
}

func (r *nodeRegistry) nodesContainingService(svcKey serviceKey) []nodeInfo {
	r.nodesMu.RLock()
	defer r.nodesMu.RUnlock()
	r.svcNodesMu.RLock()
	defer r.svcNodesMu.RUnlock()

	var nodes []nodeInfo

	svcNodes := r.svcNodes[svcKey]
	for nodeName := range svcNodes {
		node := r.nodes[nodeName]
		if node != nil {
			nodes = append(nodes, *node)
		}
	}

	return nodes
}

func (r *nodeRegistry) updateEndpointsNodes(endpoints *v1.Endpoints) {
	r.svcNodesMu.Lock()
	defer r.svcNodesMu.Unlock()

	key := serviceKey{namespace: endpoints.Namespace, name: endpoints.Name}

	nodeSet := sets.String{}
	for _, subset := range endpoints.Subsets {
		for _, addr := range subset.Addresses {
			if addr.NodeName == nil {
				continue
			}
			nodeSet.Insert(*addr.NodeName)
		}
	}

	r.svcNodes[key] = nodeSet
}

func (r *nodeRegistry) deleteEndpointsNodes(endpoints *v1.Endpoints) {
	r.svcNodesMu.Lock()
	defer r.svcNodesMu.Unlock()

	key := serviceKey{namespace: endpoints.Namespace, name: endpoints.Name}
	delete(r.svcNodes, key)
}

func (r *nodeRegistry) handleEndpoints() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			endpoints, ok := obj.(*v1.Endpoints)
			if !ok {
				return
			}
			r.updateEndpointsNodes(endpoints)
		},
		UpdateFunc: func(_, obj interface{}) {
			endpoints, ok := obj.(*v1.Endpoints)
			if !ok {
				return
			}
			r.updateEndpointsNodes(endpoints)
		},
		DeleteFunc: func(obj interface{}) {
			if endpoints, ok := obj.(*v1.Endpoints); ok {
				r.deleteEndpointsNodes(endpoints)
				return
			}
			if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				if endpoints, ok := tombstone.Obj.(*v1.Endpoints); ok {
					r.deleteEndpointsNodes(endpoints)
				}
			}
		},
	}
}

func (r *nodeRegistry) nodesForService(svc *v1.Service) ([]nodeInfo, error) {
	r.nodesMu.RLock()
	defer r.nodesMu.RUnlock()

	if svc == nil {
		return nil, errors.New("service cannot be nil")
	}

	var nodes []nodeInfo

	var filterValue string
	if r.cs.config.Global.ServiceFilterLabel != "" {
		filterValue, _ = getLabelOrAnnotation(svc.ObjectMeta, r.cs.config.Global.ServiceFilterLabel)
	}

	environment := r.cs.environmentForMeta(svc.ObjectMeta)
	project, _ := r.cs.projectForMeta(svc.ObjectMeta, environment)

	for _, nInfo := range r.nodes {
		if nInfo.environmentID != environment {
			continue
		}
		if filterValue != "" && nInfo.filterValue != filterValue {
			continue
		}
		if project != "" && nInfo.projectID != project {
			continue
		}

		nodes = append(nodes, *nInfo)
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes available to add to service %s/%s", svc.Namespace, svc.Name)
	}

	return nodes, nil
}

func (r *nodeRegistry) idsForService(svc *v1.Service) (hostIDs []string, networkIDs []string, projectID string, err error) {
	nodes, err := r.nodesForService(svc)
	if err != nil {
		return nil, nil, "", err
	}

	hostIDs, networkIDs, projectID = idsForNodes(nodes)
	return hostIDs, networkIDs, projectID, nil
}

func idsForNodes(nodes []nodeInfo) (hostIDs []string, networkIDs []string, projectID string) {
	networkSet := sets.String{}

	for _, nInfo := range nodes {
		hostIDs = append(hostIDs, nInfo.vmID)
		networkSet.Insert(nInfo.networkID)
		projectID = nInfo.projectID
	}

	return hostIDs, networkSet.List(), projectID
}

func (r *nodeRegistry) updateNodes(nodes []*v1.Node) error {
	if len(nodes) == 0 {
		return errors.New("no nodes available to add to load balancer")
	}

	r.nodesMu.Lock()
	defer r.nodesMu.Unlock()

	r.revision++

	visitedNodes := sets.String{}
	for _, n := range nodes {
		visitedNodes.Insert(n.Name)
		if _, ok := r.nodes[n.Name]; ok {
			err := r.nodes[n.Name].updateLabels(r.cs, n)
			if err != nil {
				return err
			}
			continue
		}

		nInfo, err := newNodeInfo(r.cs, n)
		if err != nil {
			return err
		}
		if nInfo == nil {
			continue
		}
		nInfo.revision = r.revision
		r.nodes[n.Name] = nInfo
	}

	toRemove := sets.StringKeySet(r.nodes).Difference(visitedNodes)
	for k := range toRemove {
		delete(r.nodes, k)
	}

	if len(r.nodes) == 0 {
		return fmt.Errorf("unable to map kubernetes nodes to cloudstack instances for nodes: %v", nodeNames(nodes))
	}

	return nil
}

func (n *nodeInfo) updateLabels(cs *CSCloud, node *v1.Node) error {
	n.environmentID = cs.environmentForMeta(node.ObjectMeta)

	if cs.config.Global.NodeFilterLabel != "" {
		n.filterValue, _ = getLabelOrAnnotation(node.ObjectMeta, cs.config.Global.NodeFilterLabel)
	}

	var err error
	n.projectID, err = cs.projectForMeta(node.ObjectMeta, n.environmentID)
	if err != nil {
		return fmt.Errorf("unable to retrieve projectID for node %q in environment %q: %v", node.Name, n.environmentID, err)
	}

	n.hostName = node.Name
	if name, ok := getLabelOrAnnotation(node.ObjectMeta, cs.config.Global.NodeNameLabel); ok {
		n.hostName = name
	}

	return nil
}

func newNodeInfo(cs *CSCloud, node *v1.Node) (*nodeInfo, error) {
	nInfo := nodeInfo{
		name: node.Name,
	}

	err := nInfo.updateLabels(cs, node)
	if err != nil {
		return nil, err
	}

	var manager *cloudstackManager
	if env, ok := cs.environments[nInfo.environmentID]; ok {
		manager = env.manager
	}
	if manager == nil {
		return nil, fmt.Errorf("unable to retrieve cloudstack manager for environment %q", nInfo.environmentID)
	}

	vm, err := manager.virtualMachineByName(nInfo.hostName, nInfo.projectID)
	if err != nil && err != ErrVMNotFound {
		return nil, err
	}
	if err == ErrVMNotFound {
		// Ignore nodes not found in cloudstack, they should be soon removed.
		return nil, nil
	}

	nic, err := cs.externalNIC(vm)
	if err != nil {
		return nil, err
	}

	nInfo.vmID = vm.Id
	nInfo.networkID = nic.Networkid

	return &nInfo, nil
}
