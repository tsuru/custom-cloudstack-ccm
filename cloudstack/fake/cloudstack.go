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
	"github.com/xanzy/go-cloudstack/v2/cloudstack"
)

type MockAPICall struct {
	Command string
	Params  url.Values
}

type LoadBalancerRule struct {
	Rule         map[string]interface{}
	Pools        []globoNetworkPool
	createLBPool bool
}

type globoNetworkPoolsResponse struct {
	Count             int                 `json:"count"`
	GloboNetworkPools []*globoNetworkPool `json:"globonetworkpool"`
}

type UpdateGloboNetworkPoolResponse struct {
	JobID string `json:"jobid"`
}

type globoNetworkPool struct {
	Port                int    `json:"port"`
	VipPort             int    `json:"vipport"`
	HealthCheckType     string `json:"healthchecktype"`
	HealthCheck         string `json:"healthcheck"`
	HealthCheckExpected string `json:"healthcheckexpected"`
	Id                  int    `json:"id"`
}

type CloudstackServer struct {
	*httptest.Server
	Calls   []MockAPICall
	Hook    func(w http.ResponseWriter, r *http.Request) bool
	idx     map[string]int
	Jobs    map[string]func() interface{}
	tags    map[string][]cloudstack.Tags
	lbRules map[string]*LoadBalancerRule
	ips     map[string]*cloudstack.PublicIpAddress
	vms     map[string][]*cloudstack.VirtualMachine
}

func NewCloudstackServer() *CloudstackServer {
	cloudstackSrv := &CloudstackServer{
		idx:     make(map[string]int),
		lbRules: make(map[string]*LoadBalancerRule),
		Jobs:    make(map[string]func() interface{}),
		tags:    make(map[string][]cloudstack.Tags),
		ips:     make(map[string]*cloudstack.PublicIpAddress),
		vms:     make(map[string][]*cloudstack.VirtualMachine),
	}
	cloudstackSrv.Server = httptest.NewServer(cloudstackSrv)
	return cloudstackSrv
}

func (s *CloudstackServer) newID(cmd string) int {
	s.idx[cmd]++
	return s.idx[cmd]
}

func (s *CloudstackServer) lbNameByID(lbID string) string {
	for k, lb := range s.lbRules {
		if lb.Rule["id"] == lbID {
			return k
		}
	}
	return ""
}

func (s *CloudstackServer) AddIP(ip cloudstack.PublicIpAddress) {
	s.ips[ip.Id] = &ip
}

func (s *CloudstackServer) AddLBRule(lbName string, lbRule LoadBalancerRule) {
	s.lbRules[lbName] = &lbRule
}

func (s *CloudstackServer) AddTags(resourceid string, tags []cloudstack.Tags) {
	s.tags[resourceid] = tags
}

func (s *CloudstackServer) SetDefaultLBPoolCreation(lbId string) {
	lbName := s.lbNameByID(lbId)
	lbRule := s.lbRules[lbName]
	lbRule.createLBPool = !lbRule.createLBPool
	s.lbRules[lbName] = lbRule
}

func (s *CloudstackServer) createDefaultLBPools(lbId string) {
	lbName := s.lbNameByID(lbId)
	lbRule := s.lbRules[lbName]
	publicPort, _ := strconv.Atoi(lbRule.Rule["publicport"].(string))
	privatePort, _ := strconv.Atoi(lbRule.Rule["privateport"].(string))
	protocol := lbRule.Rule["protocol"].(string)
	createDefaultLBPool := lbRule.createLBPool
	if len(lbRule.Pools) > 1 || !createDefaultLBPool {
		return
	}
	lbRule.Pools = append(lbRule.Pools, globoNetworkPool{
		Id:              0,
		VipPort:         publicPort,
		Port:            privatePort,
		HealthCheckType: protocol,
	})
	additionalPortMap, ok := lbRule.Rule["additionalportmap"].([]string)
	if !ok {
		return
	}
	for idx, portPair := range additionalPortMap {
		if !strings.Contains(portPair, ":") {
			continue
		}
		additionalPublicPort, _ := strconv.Atoi(strings.Split(portPair, ":")[0])
		additionalPrivatePort, _ := strconv.Atoi(strings.Split(portPair, ":")[1])
		lbRule.Pools = append(lbRule.Pools, globoNetworkPool{
			Id:              idx + 1,
			VipPort:         additionalPublicPort,
			Port:            additionalPrivatePort,
			HealthCheckType: protocol,
		})
	}
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
		queryTags := parseTags(r.Form)

		var lbs []map[string]interface{}
		for _, lb := range s.lbRules {
			lbTags := s.tags[lb.Rule["id"].(string)]
			matchTags := false
			for _, tag := range lbTags {
				if queryTags[tag.Key] == tag.Value {
					matchTags = true
				}
			}
			if matchTags ||
				(keyword != "" && (strings.Contains(lb.Rule["name"].(string), keyword) || strings.Contains(lb.Rule["id"].(string), keyword))) {
				lb.Rule["tags"] = lbTags
				lbs = append(lbs, lb.Rule)
			}
		}

		w.Write(MarshalResponse("listLoadBalancerRulesResponse", map[string]interface{}{
			"count":            len(lbs),
			"loadbalancerrule": lbs,
		}))

	case "listGloboNetworkPools":
		lbruleid := r.FormValue("lbruleid")
		s.createDefaultLBPools(lbruleid)
		for _, lb := range s.lbRules {
			if lb.Rule["id"] == lbruleid {
				w.Write(MarshalResponse("listGloboNetworkPoolResponse", map[string]interface{}{
					"count":            len(lb.Pools),
					"globonetworkpool": lb.Pools,
				}))
				return
			}
		}
		w.Write(MarshalResponse("listGloboNetworkPoolResponse", globoNetworkPoolsResponse{
			Count: 0,
		}))

	case "listNetworks":
		w.Write([]byte(`{"listNetworksResponse": {"count": 1, "network": [{"id": "net1"}]}}`))

	case "listPublicIpAddresses":
		r.ParseForm()
		address := r.FormValue("ipaddress")
		id := r.FormValue("id")
		page, _ := strconv.Atoi(r.FormValue("page"))
		if page > 1 {
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
			if id != "" && ip.Id != id {
				continue
			}
			includeIP := len(tags) == 0
			for _, tag := range s.tags[ip.Id] {
				if tags[tag.Key] == tag.Value {
					includeIP = true
				}
				ip.Tags = append(ip.Tags, tag)
			}
			if includeIP {
				ips = append(ips, ip)
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
		s.Jobs[obj.JobID] = func() interface{} {
			obj.Ipaddress = fmt.Sprintf("10.0.0.%d", ipIdx)
			s.ips[ipID] = &cloudstack.PublicIpAddress{
				Id:        obj.Id,
				Ipaddress: obj.Ipaddress,
			}
			return obj
		}

	case "disassociateIpAddress":
		ipID := r.FormValue("id")
		if _, ok := s.ips[ipID]; !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write(ErrorResponse("disassociateIpAddressResponse", fmt.Sprintf("ip not found: %q", ipID)))
			return
		}
		jobIdx := s.newID(cmd)
		obj := cloudstack.DisassociateIpAddressResponse{
			JobID: fmt.Sprintf("job-ip-disassociate-%d", jobIdx),
		}
		w.Write(MarshalResponse("disassociateIpAddressResponse", obj))
		s.Jobs[obj.JobID] = func() interface{} {
			delete(s.ips, ipID)
			obj.Success = true
			return obj
		}

	case "updateLoadBalancerRule":
		lbID := r.FormValue("id")
		lbName := s.lbNameByID(lbID)
		if lbName == "" {
			w.WriteHeader(http.StatusNotFound)
			w.Write(ErrorResponse("updateLoadBalancerRuleResponse", fmt.Sprintf("lb not found with id %v", lbID)))
			return
		}
		ruleIdx := s.newID(cmd)
		algorithm := r.FormValue("algorithm")
		obj := cloudstack.UpdateLoadBalancerRuleResponse{
			JobID: fmt.Sprintf("job-lbrule-update-%d", ruleIdx),
		}
		w.Write(MarshalResponse("updateLoadBalancerRuleResponse", obj))
		s.Jobs[obj.JobID] = func() interface{} {
			s.lbRules[lbName].Rule["algorithm"] = algorithm
			return s.lbRules[lbName]
		}

	case "updateGloboNetworkPool":
		lbRuleID := r.FormValue("lbruleid")
		poolID, _ := strconv.Atoi(r.FormValue("poolids"))
		healthCheckType := r.FormValue("healthchecktype")
		healthCheckProtocol := r.FormValue("healthcheck")
		healthCheckExpected := r.FormValue("expectedhealthcheck")

		lbName := s.lbNameByID(lbRuleID)
		if lbName == "" {
			w.WriteHeader(http.StatusNotFound)
			w.Write(ErrorResponse("updateGloboNetworkPoolResponse", fmt.Sprintf("lb not found with id %v", lbRuleID)))
			return
		}
		if len(s.lbRules[lbName].Pools) == 0 {
			w.WriteHeader(http.StatusNotFound)
			w.Write(ErrorResponse("updateGloboNetworkPoolResponse", fmt.Sprintf("pool %v for lb %v not found", lbRuleID, lbName)))
			return
		}
		for idx, pool := range s.lbRules[lbName].Pools {
			if pool.Id == poolID {
				ruleIdx := s.newID(cmd)
				obj := UpdateGloboNetworkPoolResponse{
					JobID: fmt.Sprintf("job-lbrule-pool-update-%d", ruleIdx),
				}
				w.Write(MarshalResponse("updateGloboNetworkPoolResponse", obj))
				s.Jobs[obj.JobID] = func() interface{} {
					s.lbRules[lbName].Pools[idx].HealthCheck = healthCheckProtocol
					s.lbRules[lbName].Pools[idx].HealthCheckType = healthCheckType
					s.lbRules[lbName].Pools[idx].HealthCheckExpected = healthCheckExpected
					return s.lbRules[lbName]
				}
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write(ErrorResponse("updateGloboNetworkPoolResponse", fmt.Sprintf("pool %v for lb %v not found", lbRuleID, lbName)))
		return

	case "createLoadBalancerRule":
		lbname := r.FormValue("name")
		if _, ok := s.lbRules[lbname]; ok {
			w.WriteHeader(http.StatusConflict)
			w.Write(ErrorResponse("createLoadBalancerRuleResponse", fmt.Sprintf("lb already exists with name %v", lbname)))
			return
		}
		ruleIdx := s.newID(cmd)
		jobID := fmt.Sprintf("job-lbrule-%d", ruleIdx)
		obj := LoadBalancerRule{
			createLBPool: true,
			Rule: map[string]interface{}{
				"id":    fmt.Sprintf("lbrule-%d", ruleIdx),
				"jobid": jobID},
		}
		ipObj := s.ips[r.FormValue("publicipid")]
		if ipObj.Id == "" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write(ErrorResponse(cmd+"Response", fmt.Sprintf("ip id not found: %s", r.FormValue("publicipid"))))
			return
		}
		w.Write(MarshalResponse("createLoadBalancerRuleResponse", obj.Rule))
		obj.Rule["algorithm"] = r.FormValue("algorithm")
		obj.Rule["name"] = r.FormValue("name")
		obj.Rule["privateport"] = r.FormValue("privateport")
		obj.Rule["publicport"] = r.FormValue("publicport")
		obj.Rule["networkid"] = r.FormValue("networkid")
		obj.Rule["publicipid"] = r.FormValue("publicipid")
		obj.Rule["protocol"] = r.FormValue("protocol")
		obj.Rule["openfirewall"], _ = strconv.ParseBool(r.FormValue("openfirewall"))
		obj.Rule["publicip"] = ipObj.Ipaddress
		if additionalPorts := r.FormValue("additionalportmap"); additionalPorts != "" {
			obj.Rule["additionalportmap"] = strings.Split(additionalPorts, ",")
		}
		s.Jobs[jobID] = func() interface{} {
			s.lbRules[lbname] = &obj
			return obj.Rule
		}

	case "assignNetworkToLBRule":
		netAssignIdx := s.newID(cmd)
		fullID := fmt.Sprintf("job-net-assign-%d", netAssignIdx)
		obj := map[string]interface{}{
			"jobid": fullID,
		}
		w.Write(MarshalResponse("assignNetworkToLBRuleResponse", obj))
		s.Jobs[fullID] = func() interface{} {
			return obj
		}

	case "createTags":
		tagsIdx := s.newID(cmd)
		obj := cloudstack.CreateTagsResponse{
			JobID: fmt.Sprintf("job-tags-%d", tagsIdx),
		}
		w.Write(MarshalResponse("createTags", obj))
		s.Jobs[obj.JobID] = func() interface{} {
			s.tags[r.FormValue("resourceids")] = append(s.tags[r.FormValue("resourceids")], cloudstack.Tags{
				Key:   r.FormValue("tags[0].key"),
				Value: r.FormValue("tags[0].value"),
			})
			return obj
		}

	case "deleteTags":
		jobID := s.newID(cmd)
		response := cloudstack.DeleteTagsResponse{
			JobID: fmt.Sprintf("job-delete-tags-%d", jobID),
		}

		w.Write(MarshalResponse("deleteTags", response))

		s.Jobs[response.JobID] = func() interface{} {
			ids := r.Form["resourceids"]
			for _, id := range ids {
				delete(s.tags, id)
			}
			response.Success = true
			return response
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
		s.Jobs[obj.JobID] = func() interface{} {
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
		for _, tag := range tags {
			if keyFilter != "" && tag.Key != keyFilter {
				continue
			}
			ptrTags = append(ptrTags, &cloudstack.Tag{
				Key:   tag.Key,
				Value: tag.Value,
			})
		}
		w.Write(MarshalResponse("listTagsResponse", cloudstack.ListTagsResponse{
			Count: len(ptrTags),
			Tags:  ptrTags,
		}))

	case "deleteLoadBalancerRule":
		lbID := r.FormValue("id")
		deleteIdx := s.newID(cmd)
		obj := cloudstack.DeleteLoadBalancerRuleResponse{
			JobID: fmt.Sprintf("job-delete-lb-%d", deleteIdx),
		}
		w.Write(MarshalResponse("deleteLoadBalancerRuleResponse", obj))
		s.Jobs[obj.JobID] = func() interface{} {
			lbName := s.lbNameByID(lbID)
			delete(s.lbRules, lbName)
			delete(s.tags, lbID)
			return obj
		}

	case "listLoadBalancerRuleInstances":
		page, _ := strconv.Atoi(r.FormValue("page"))
		if page > 1 {
			w.Write(MarshalResponse("listLoadBalancerRuleInstancesResponse", cloudstack.ListLoadBalancerRuleInstancesResponse{
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
		callback := s.Jobs[jobID]
		if callback == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write(ErrorResponse("queryAsyncJobResultResponse", fmt.Sprintf("job id %q not found: %#v", jobID, r.URL.Query())))
			break
		}
		w.Write(MarshalResponse("queryAsyncJobResultResponse", map[string]interface{}{
			"jobstatus": 1,
			"jobresult": map[string]interface{}{
				"cmdResponse": callback(),
			},
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
	for k := range form {
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
