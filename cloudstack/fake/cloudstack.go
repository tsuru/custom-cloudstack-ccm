package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xanzy/go-cloudstack/cloudstack"
)

type MockAPICall struct {
	Command string
	Params  url.Values
}

type loadBalancerRule map[string]interface{}

type CloudstackServer struct {
	*httptest.Server
	Calls   []MockAPICall
	Hook    func(w http.ResponseWriter, r *http.Request) bool
	idx     map[string]int
	jobs    map[string]func() interface{}
	tags    map[string][]cloudstack.Tag
	lbRules map[string]loadBalancerRule
	ips     map[string]cloudstack.PublicIpAddress
	vms     map[string][]*cloudstack.VirtualMachine
}

func NewCloudstackServer() *CloudstackServer {
	cloudstackSrv := &CloudstackServer{
		idx:     make(map[string]int),
		lbRules: make(map[string]loadBalancerRule),
		jobs:    make(map[string]func() interface{}),
		tags:    make(map[string][]cloudstack.Tag),
		ips:     make(map[string]cloudstack.PublicIpAddress),
		vms:     make(map[string][]*cloudstack.VirtualMachine),
	}
	cloudstackSrv.Server = httptest.NewServer(cloudstackSrv)
	return cloudstackSrv
}

func (s *CloudstackServer) newID(cmd string) int {
	s.idx[cmd]++
	return s.idx[cmd]
}

func (s *CloudstackServer) lastID(cmd string) int {
	return s.idx[cmd]
}

func (s *CloudstackServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cmd := r.FormValue("command")
	s.Calls = append(s.Calls, MockAPICall{
		Command: cmd,
		Params:  r.URL.Query(),
	})
	if s.Hook != nil && s.Hook(w, r) {
		return
	}
	switch cmd {
	case "listVirtualMachines":
		w.Write([]byte(`{"listVirtualMachinesResponse": {"count": 1, "virtualmachine": [{"name": "n1", "id": "vm1", "nic": [{"networkid": "net1"}]}]}}`))

	case "listLoadBalancerRules":
		keyword := r.FormValue("keyword")
		var lbs []loadBalancerRule
		for _, lb := range s.lbRules {
			if lb["name"] == keyword || lb["id"] == keyword {
				lb["tags"] = s.tags[lb["id"].(string)]
				lbs = append(lbs, lb)
			}
		}
		w.Write(MarshalResponse("listLoadBalancerRulesResponse", map[string]interface{}{
			"count":            len(lbs),
			"loadbalancerrule": lbs,
		}))

	case "listNetworks":
		w.Write([]byte(`{"listNetworksResponse": {"count": 1, "network": [{"id": "net1"}]}}`))

	case "listPublicIpAddresses":
		r.ParseForm()
		address := r.FormValue("ipaddress")
		page, _ := strconv.Atoi(r.FormValue("page"))
		if page > 0 {
			w.Write(MarshalResponse("listPublicIpAddressesResponse", cloudstack.ListPublicIpAddressesResponse{
				Count: 0,
			}))
			return
		}
		tags := parseTags(r.Form)
		var ips []*cloudstack.PublicIpAddress
		for _, ip := range s.ips {
			if address != "" && ip.Ipaddress != address {
				continue
			}
			includeIP := len(tags) == 0
			for _, tag := range s.tags[ip.Id] {
				if tags[tag.Key] == tag.Value {
					includeIP = true
				}
				ip.Tags = append(ip.Tags, cloudstack.PublicIpAddressTags(tag))
			}
			if includeIP {
				ips = append(ips, &ip)
			}
		}
		w.Write(MarshalResponse("listPublicIpAddressesResponse", cloudstack.ListPublicIpAddressesResponse{
			Count:             len(ips),
			PublicIpAddresses: ips,
		}))

	case "associateIpAddress":
		ipIdx := s.newID(cmd)
		ipID := fmt.Sprintf("ip-%d", ipIdx)
		obj := cloudstack.AssociateIpAddressResponse{
			Id:    ipID,
			JobID: fmt.Sprintf("job-ip-%d", ipIdx),
		}
		w.Write(MarshalResponse("associateIpAddressResponse", obj))
		s.jobs[obj.JobID] = func() interface{} {
			obj.Ipaddress = fmt.Sprintf("10.0.0.%d", ipIdx)
			s.ips[ipID] = cloudstack.PublicIpAddress{
				Id:        obj.Id,
				Ipaddress: obj.Ipaddress,
			}
			return obj
		}

	case "createLoadBalancerRule":
		lbname := r.FormValue("name")
		if _, ok := s.lbRules[lbname]; ok {
			w.WriteHeader(http.StatusConflict)
			w.Write(ErrorResponse("createLoadBalancerRule", fmt.Sprintf("lb already exists with name %v", lbname)))
			return
		}
		ruleIdx := s.newID(cmd)
		jobID := fmt.Sprintf("job-lbrule-%d", ruleIdx)
		obj := loadBalancerRule{
			"id":    fmt.Sprintf("lbrule-%d", ruleIdx),
			"jobid": jobID,
		}
		ipObj := s.ips[r.FormValue("publicipid")]
		if ipObj.Id == "" {
			w.Write(ErrorResponse(cmd+"Response", fmt.Sprintf("ip id not found: %s", r.FormValue("publicipid"))))
			return
		}
		w.Write(MarshalResponse("createLoadBalancerRuleResponse", obj))
		obj["algorithm"] = r.FormValue("algorithm")
		obj["name"] = r.FormValue("name")
		obj["privateport"] = r.FormValue("privateport")
		obj["publicport"] = r.FormValue("publicport")
		obj["networkid"] = r.FormValue("networkid")
		obj["publicipid"] = r.FormValue("publicipid")
		obj["protocol"] = r.FormValue("protocol")
		obj["openfirewall"], _ = strconv.ParseBool(r.FormValue("openfirewall"))
		obj["publicip"] = ipObj.Ipaddress
		if additionalPorts := r.FormValue("additionalportmap"); additionalPorts != "" {
			obj["additionalportmap"] = strings.Split(r.FormValue("additionalportmap"), ",")
		}
		s.jobs[jobID] = func() interface{} {
			s.lbRules[lbname] = obj
			return obj
		}

	case "assignNetworkToLBRule":
		netAssignIdx := s.newID(cmd)
		fullID := fmt.Sprintf("job-net-assign-%d", netAssignIdx)
		obj := map[string]interface{}{
			"jobid": fullID,
		}
		w.Write(MarshalResponse("assignNetworkToLBRuleResponse", obj))
		s.jobs[fullID] = func() interface{} {
			return obj
		}

	case "createTags":
		tagsIdx := s.newID(cmd)
		obj := cloudstack.CreateTagsResponse{
			JobID: fmt.Sprintf("job-tags-%d", tagsIdx),
		}
		w.Write(MarshalResponse("createTags", obj))
		s.jobs[obj.JobID] = func() interface{} {
			s.tags[r.FormValue("resourceids")] = append(s.tags[r.FormValue("resourceids")], cloudstack.Tag{
				Key:   r.FormValue("tags[0].key"),
				Value: r.FormValue("tags[0].value"),
			})
			return obj
		}

	case "assignToLoadBalancerRule":
		ruleId := r.FormValue("id")
		vms := r.FormValue("virtualmachineids")
		var vmIDs []string
		if vms != "" {
			vmIDs = strings.Split(vms, ",")
		}
		hostAssignIdx := s.newID(cmd)
		obj := cloudstack.AssignToLoadBalancerRuleResponse{
			JobID: fmt.Sprintf("job-host-assign-%d", hostAssignIdx),
		}
		w.Write(MarshalResponse("assignToLoadBalancerRuleResponse", obj))
		s.jobs[obj.JobID] = func() interface{} {
			for _, vmID := range vmIDs {
				s.vms[ruleId] = append(s.vms[ruleId], &cloudstack.VirtualMachine{
					Id: vmID,
				})
			}
			return obj
		}

	case "listTags":
		keyFilter := r.FormValue("key")
		tags := s.tags[r.FormValue("resourceid")]
		var ptrTags []*cloudstack.Tag
		for i, tag := range tags {
			if keyFilter != "" && tag.Key != keyFilter {
				continue
			}
			ptrTags = append(ptrTags, &tags[i])
		}
		w.Write(MarshalResponse("listTagsResponse", cloudstack.ListTagsResponse{
			Count: len(tags),
			Tags:  ptrTags,
		}))

	case "deleteLoadBalancerRule":
		lbID := r.FormValue("id")
		deleteIdx := s.newID(cmd)
		obj := cloudstack.DeleteLoadBalancerRuleResponse{
			JobID: fmt.Sprintf("job-delete-lb-%d", deleteIdx),
		}
		w.Write(MarshalResponse("deleteLoadBalancerRuleResponse", obj))
		s.jobs[obj.JobID] = func() interface{} {
			for k, lb := range s.lbRules {
				if lb["id"] == lbID {
					delete(s.lbRules, k)
					break
				}
			}
			delete(s.tags, lbID)
			return obj
		}

	case "listLoadBalancerRuleInstances":
		page, _ := strconv.Atoi(r.FormValue("page"))
		if page > 0 {
			w.Write(MarshalResponse("listPublicIpAddressesResponse", cloudstack.ListPublicIpAddressesResponse{
				Count: 0,
			}))
			return
		}
		vms := s.vms[r.FormValue("id")]
		w.Write(MarshalResponse("listLoadBalancerRuleInstancesResponse", cloudstack.ListLoadBalancerRuleInstancesResponse{
			Count:                     len(vms),
			LoadBalancerRuleInstances: vms,
		}))

	case "queryAsyncJobResult":
		jobID := r.FormValue("jobid")
		callback := s.jobs[jobID]
		if callback == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write(ErrorResponse("queryAsyncJobResultResponse", fmt.Sprintf("job id %q not found: %#v", jobID, r.URL.Query())))
			break
		}
		w.Write(MarshalResponse("queryAsyncJobResultResponse", map[string]interface{}{
			"jobstatus": 1,
			"jobresult": callback(),
		}))

	default:
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(ErrorResponse(cmd+"Response", fmt.Sprintf("fake call for %q not implemented, args: %#v", cmd, r.URL.Query())))
	}
}

func ErrorResponse(cmd, msg string) []byte {
	return MarshalResponse(cmd, cloudstack.CSError{
		ErrorCode:   999,
		CSErrorCode: 999,
		ErrorText:   msg,
	})
}

func MarshalResponse(name string, obj interface{}) []byte {
	data, _ := json.Marshal(map[string]interface{}{
		name: obj,
	})
	return data
}

func (s *CloudstackServer) HasCalls(t *testing.T, calls []MockAPICall) {
	callsError := []string{
		"expected calls:",
	}
	for _, call := range calls {
		callsError = append(callsError, "  "+call.Command)
	}
	callsError = append(callsError, "actual calls:")
	for _, call := range s.Calls {
		callsError = append(callsError, "  "+call.Command)
	}

	if !assert.Len(t, s.Calls, len(calls), strings.Join(callsError, "\n")) {
		return
	}

	for i, expectedCall := range calls {
		actualCall := s.Calls[i]
		assert.Equal(t, actualCall.Command, expectedCall.Command)
		for k, v := range expectedCall.Params {
			if v == nil {
				assert.Contains(t, actualCall.Params, k, "existing params: %#v", actualCall.Params)
			} else {
				assert.Equal(t, actualCall.Params[k], v, "existing params: %#v", actualCall.Params)
			}
		}
	}
}

func parseTags(form url.Values) map[string]string {
	tagRegexp := regexp.MustCompile(`tags\[(\d+)\]\.(key|value)`)
	keys := map[string]string{}
	values := map[string]string{}
	for k, _ := range form {
		matches := tagRegexp.FindStringSubmatch(k)
		if len(matches) != 3 {
			continue
		}
		if matches[2] == "key" {
			keys[matches[1]] = form.Get(k)
		} else if matches[2] == "value" {
			values[matches[1]] = form.Get(k)
		}
	}
	result := map[string]string{}
	for id, k := range keys {
		result[k] = values[id]
	}
	return result
}
