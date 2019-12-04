package cloudstack

import (
	"github.com/xanzy/go-cloudstack/cloudstack"
)

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
