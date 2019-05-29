package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	jobs    map[string]interface{}
	tags    map[string][]cloudstack.Tag
	lbRules map[string]loadBalancerRule
	ips     map[string]cloudstack.AssociateIpAddressResponse
}

func NewCloudstackServer() *CloudstackServer {
	cloudstackSrv := &CloudstackServer{
		idx:     make(map[string]int),
		lbRules: make(map[string]loadBalancerRule),
		jobs:    make(map[string]interface{}),
		tags:    make(map[string][]cloudstack.Tag),
		ips:     make(map[string]cloudstack.AssociateIpAddressResponse),
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
		w.Write(marshalResponse("listLoadBalancerRulesResponse", map[string]interface{}{
			"count":            len(lbs),
			"loadbalancerrule": lbs,
		}))

	case "listNetworks":
		w.Write([]byte(`{"listNetworksResponse": {"count": 1, "network": [{"id": "net1"}]}}`))

	case "associateIpAddress":
		ipIdx := s.newID(cmd)
		ipID := fmt.Sprintf("ip-%d", ipIdx)
		obj := cloudstack.AssociateIpAddressResponse{
			Id:    ipID,
			JobID: fmt.Sprintf("job-ip-%d", ipIdx),
		}
		w.Write(marshalResponse("associateIpAddressResponse", obj))
		obj.Ipaddress = fmt.Sprintf("10.0.0.%d", ipIdx)
		s.ips[ipID] = obj
		s.jobs[obj.JobID] = obj

	case "createLoadBalancerRule":
		lbname := r.FormValue("name")
		if _, ok := s.lbRules[lbname]; ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		ruleID := s.newID(cmd)
		jobID := fmt.Sprintf("job-lbrule-%d", ruleID)
		obj := loadBalancerRule{
			"id":    fmt.Sprintf("lbrule-%d", ruleID),
			"jobid": jobID,
		}
		ipObj := s.ips[r.FormValue("publicipid")]
		if ipObj.Id == "" {
			w.Write(errorResponse(cmd+"Response", fmt.Sprintf("ip id not found", r.FormValue("publicipid"))))
			return
		}
		w.Write(marshalResponse("createLoadBalancerRuleResponse", obj))
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
		s.jobs[jobID] = obj
		s.lbRules[lbname] = obj

	case "assignNetworkToLBRule":
		netAssignID := s.newID(cmd)
		fullID := fmt.Sprintf("job-net-assign-%d", netAssignID)
		obj := map[string]interface{}{
			"jobid": fullID,
		}
		w.Write(marshalResponse("assignNetworkToLBRuleResponse", obj))
		s.jobs[fullID] = obj

	case "createTags":
		tagsID := s.newID(cmd)
		obj := cloudstack.CreateTagsResponse{
			JobID: fmt.Sprintf("job-tags-%d", tagsID),
		}
		w.Write(marshalResponse("createTags", obj))
		s.tags[r.FormValue("resourceids")] = append(s.tags[r.FormValue("resourceids")], cloudstack.Tag{
			Key:   r.FormValue("tags[0].key"),
			Value: r.FormValue("tags[0].value"),
		})
		s.jobs[obj.JobID] = obj

	case "assignToLoadBalancerRule":
		hostAssignID := s.newID(cmd)
		obj := cloudstack.AssignToLoadBalancerRuleResponse{
			JobID: fmt.Sprintf("job-host-assign-%d", hostAssignID),
		}
		w.Write(marshalResponse("assignToLoadBalancerRuleResponse", obj))
		s.jobs[obj.JobID] = obj

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
		w.Write(marshalResponse("listTagsResponse", cloudstack.ListTagsResponse{
			Count: len(tags),
			Tags:  ptrTags,
		}))

	case "queryAsyncJobResult":
		jobID := r.FormValue("jobid")
		obj := s.jobs[jobID]
		if obj == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write(errorResponse("queryAsyncJobResultResponse", fmt.Sprintf("job id %q not found: %#v", jobID, r.URL.Query())))
			break
		}
		w.Write(marshalResponse("queryAsyncJobResultResponse", map[string]interface{}{
			"jobstatus": 1,
			"jobresult": obj,
		}))

	default:
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(errorResponse(cmd+"Response", fmt.Sprintf("fake call for %q not implemented, args: %#v", cmd, r.URL.Query())))
	}
}

func errorResponse(cmd, msg string) []byte {
	return marshalResponse(cmd, cloudstack.CSError{
		ErrorCode:   999,
		CSErrorCode: 999,
		ErrorText:   msg,
	})
}

func marshalResponse(name string, obj interface{}) []byte {
	data, _ := json.Marshal(map[string]interface{}{
		name: obj,
	})
	return data
}

func (s *CloudstackServer) HasCalls(t *testing.T, calls []MockAPICall) {
	require.Len(t, s.Calls, len(calls))
	for i, expectedCall := range calls {
		actualCall := s.Calls[i]
		assert.Equal(t, actualCall.Command, expectedCall.Command)
		for k, v := range expectedCall.Params {
			assert.Equal(t, actualCall.Params[k], v, "existing params: %#v", actualCall.Params)
		}
	}
}
