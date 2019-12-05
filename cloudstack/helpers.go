package cloudstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dgraph-io/ristretto"
	"github.com/xanzy/go-cloudstack/cloudstack"
)

func vmCacheKey(name, projectID string) string {
	return strings.Join([]string{name, projectID}, "\x00")
}

var ErrVMNotFound = errors.New("vm not found in cloudstack")

type cloudstackManager struct {
	client *cloudstack.CloudStackClient
	cache  *ristretto.Cache
}

func newCloudstackManager(client *cloudstack.CloudStackClient) (*cloudstackManager, error) {
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 100000,   // Expected maximum items set to 10000, this value is 10x this number, as recommend by the docs.
		MaxCost:     50 << 20, // Maximum cost of cache (50 MB).
		BufferItems: 64,       // The magic value of 64 is the one recommended by the docs initially.
		Cost: func(value interface{}) int64 {
			// Approximate in memory size by converting to json
			data, _ := json.Marshal(value)
			return int64(len(data))
		},
	})
	if err != nil {
		return nil, err
	}
	return &cloudstackManager{
		cache:  cache,
		client: client,
	}, nil
}

func (m *cloudstackManager) virtualMachineByName(name, projectID string) (*cloudstack.VirtualMachine, error) {
	key := vmCacheKey(name, projectID)
	if vm, found := m.cache.Get(key); found {
		return vm.(*cloudstack.VirtualMachine), nil
	}
	p := m.client.VirtualMachine.NewListVirtualMachinesParams()
	p.SetName(name)
	if projectID != "" {
		p.SetProjectid(projectID)
	}
	vmsResponse, err := m.client.VirtualMachine.ListVirtualMachines(p)
	if err != nil {
		return nil, err
	}
	if len(vmsResponse.VirtualMachines) == 0 {
		return nil, ErrVMNotFound
	}
	if len(vmsResponse.VirtualMachines) > 1 {
		return nil, fmt.Errorf("more than one machine found with name %q in project %q", name, projectID)
	}
	vm := vmsResponse.VirtualMachines[0]
	m.cache.Set(key, vm, 0)
	return vm, nil
}

type pageParams interface {
	SetPagesize(int)
	SetPage(int)
}

type resultWithID interface {
	ID() string
	Raw() interface{}
}

type vmWrapper struct {
	*cloudstack.VirtualMachine
}

func (w vmWrapper) ID() string {
	return w.Id
}

func (w vmWrapper) Raw() interface{} {
	return w.VirtualMachine
}

type ipWrapper struct {
	*cloudstack.PublicIpAddress
}

func (w ipWrapper) ID() string {
	return w.Id
}

func (w ipWrapper) Raw() interface{} {
	return w.PublicIpAddress
}

func listAllPagesUnsafe(client *cloudstack.CloudStackClient, pageSize int, params pageParams) ([]resultWithID, error) {
	params.SetPagesize(pageSize)

	page := 1

	var result []resultWithID
	var totalCount, resultSize int

	for {
		params.SetPage(page)
		switch paramsTyped := params.(type) {

		case *cloudstack.ListLoadBalancerRuleInstancesParams:
			l, err := client.LoadBalancer.ListLoadBalancerRuleInstances(paramsTyped)
			if err != nil {
				return nil, err
			}
			for _, vm := range l.LoadBalancerRuleInstances {
				result = append(result, vmWrapper{vm})
			}
			totalCount = l.Count
			resultSize = len(l.LoadBalancerRuleInstances)

		case *cloudstack.ListPublicIpAddressesParams:
			l, err := client.Address.ListPublicIpAddresses(paramsTyped)
			if err != nil {
				return nil, err
			}
			for _, ip := range l.PublicIpAddresses {
				result = append(result, ipWrapper{ip})
			}
			totalCount = l.Count
			resultSize = len(l.PublicIpAddresses)
		}

		if resultSize == 0 || len(result) >= totalCount {
			break
		}

		page++
	}

	existingSet := make(map[string]struct{})
	for i := 0; i < len(result); i++ {
		el := result[i]
		if _, ok := existingSet[el.ID()]; ok {
			result[i] = result[len(result)-1]
			result = result[:len(result)-1]
			i--
			continue
		}
		existingSet[el.ID()] = struct{}{}
	}

	return result, nil
}

func listAllLBInstancesPages(client *cloudstack.CloudStackClient, params *cloudstack.ListLoadBalancerRuleInstancesParams) ([]*cloudstack.VirtualMachine, error) {
	entries, err := listAllPagesUnsafe(client, 50, params)
	if err != nil {
		return nil, err
	}
	result := make([]*cloudstack.VirtualMachine, len(entries))
	for i := range entries {
		result[i] = entries[i].Raw().(*cloudstack.VirtualMachine)
	}
	return result, nil
}

func listAllIPPages(client *cloudstack.CloudStackClient, params *cloudstack.ListPublicIpAddressesParams) ([]*cloudstack.PublicIpAddress, error) {
	entries, err := listAllPagesUnsafe(client, 50, params)
	if err != nil {
		return nil, err
	}
	result := make([]*cloudstack.PublicIpAddress, len(entries))
	for i := range entries {
		result[i] = entries[i].Raw().(*cloudstack.PublicIpAddress)
	}
	return result, nil
}
