package cloudstack

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cloudstackFake "github.com/tsuru/custom-cloudstack-ccm/cloudstack/fake"
	"github.com/xanzy/go-cloudstack/cloudstack"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func init() {
	flag.Set("v", "10")
	flag.Set("logtostderr", "true")
}

// flow:
// create IP
// create LBRule
// create Tags
// associate Net
// associate Host

// Test cases:
// No LB, new Service, create IP success, create LB success
// No LB, new Service, create IP error - success, create LB success
// No LB, new Service, create IP success, create LB error-success
// No LB, new Service, create IP success, create LB success, assing host error-success

// Existing LB, new Service, reuse IP, reuse LB (missing tags)
// Existing LB, new Service, reuse IP, reuse LB (wrong tags?)
// Existing LB, new Service, reuse IP, reuse LB (new algorithm)
// Existing LB, new Service, reuse IP, recreate LB (new ports)

func Test_CSCloud_EnsureLoadBalancer(t *testing.T) {
	baseNodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "n1",
				Labels: map[string]string{
					"project-label":     "11111111-2222-3333-4444-555555555555",
					"environment-label": "env1",
				},
			},
		},
	}

	baseSvc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc1",
			Namespace: "myns",
			Labels: map[string]string{
				"environment-label": "env1",
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolTCP},
			},
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"app": "myapp",
			},
		},
	}

	baseAssert := func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
		require.NoError(t, err)
		assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{
				{IP: "10.0.0.1", Hostname: "svc1.test.com"},
			},
		})
		srv.HasCalls(t, []cloudstackFake.MockAPICall{
			{Command: "listVirtualMachines"},
			{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
			{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
			{Command: "listNetworks"},
			{Command: "associateIpAddress"},
			{Command: "queryAsyncJobResult"},
			{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
			{Command: "queryAsyncJobResult"},
			{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
			{Command: "queryAsyncJobResult"},
			{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
			{Command: "queryAsyncJobResult"},
			{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30001"}}},
			{Command: "queryAsyncJobResult"},
			{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
			{Command: "queryAsyncJobResult"},
			{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
			{Command: "queryAsyncJobResult"},
			{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
			{Command: "queryAsyncJobResult"},
			{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
			{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
			{Command: "queryAsyncJobResult"},
			{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
			{Command: "queryAsyncJobResult"},
		})
	}

	type consecutiveCall struct {
		svc    corev1.Service
		assert func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error)
	}

	tests := []struct {
		name  string
		hook  func(t *testing.T, srv *cloudstackFake.CloudstackServer)
		calls []consecutiveCall
	}{
		{
			name: "basic ensure",
			calls: []consecutiveCall{
				{svc: baseSvc, assert: baseAssert},
			},
		},

		{
			name: "second ensure does nothing",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},

		{
			name: "second ensure updating ports",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.Ports[0].NodePort = 30002
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "deleteLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30002"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-2"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-2"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-2"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
			},
		},

		{
			name: "create load balancer error",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				calls := 0
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "createLoadBalancerRule" && calls == 0 {
						calls++
						w.WriteHeader(http.StatusInternalServerError)
						w.Write(cloudstackFake.ErrorResponse(cmd+"Response", "my error"))
						return true
					}
					return false
				}
			},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.Error(t, err)
						require.Contains(t, err.Error(), "my error")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listPublicIpAddresses"},
							{Command: "listNetworks"},
							{Command: "associateIpAddress"},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30001"}}},
						})
					},
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"page": []string{"0"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"page": []string{"1"}}},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
			},
		},

		{
			name: "assign to load balancer error",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				calls := 0
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "assignToLoadBalancerRule" && calls == 0 {
						calls++
						w.WriteHeader(http.StatusInternalServerError)
						w.Write(cloudstackFake.ErrorResponse(cmd+"Response", "my error"))
						return true
					}
					return false
				}
			},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.Error(t, err)
						require.Contains(t, err.Error(), "my error")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listPublicIpAddresses"},
							{Command: "listNetworks"},
							{Command: "associateIpAddress"},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
						})
					},
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
			},
		},

		{
			name: "second ensure updating algorithm",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "updateLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "algorithm": []string{"source"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := cloudstackFake.NewCloudstackServer()
			defer srv.Close()
			csCloud := &CSCloud{
				environmentLabel:            "environment-label",
				projectIDLabel:              "project-label",
				kubeClient:                  fake.NewSimpleClientset(),
				customAssignNetworksCommand: "assignNetworkToLBRule",
				environments: map[string]CSEnvironment{
					"env1": {
						lbEnvironmentID: "1",
						lbDomain:        "test.com",
						client:          cloudstack.NewAsyncClient(srv.URL, "a", "b", true),
					},
				},
			}
			if tt.hook != nil {
				tt.hook(t, srv)
			}
			var lbStatus *corev1.LoadBalancerStatus
			var err error
			for i, cc := range tt.calls {
				t.Logf("call %d", i)
				svc := cc.svc.DeepCopy()
				if err == nil && lbStatus != nil {
					svc.Status.LoadBalancer = *lbStatus
				}
				lbStatus, err = csCloud.EnsureLoadBalancer("kubernetes", svc, baseNodes)
				if cc.assert != nil {
					cc.assert(t, srv, lbStatus, err)
				}
				srv.Calls = nil
			}
		})
	}
}

func TestFilterNodesMatchingLabels(t *testing.T) {
	nodes := []*v1.Node{
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "pool1"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "pool2"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "pool2"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}},
	}
	s1 := corev1.Service{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app-pool": "pool1"}}}
	s2 := corev1.Service{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app-pool": "pool2"}}}
	tt := []struct {
		name          string
		cs            CSCloud
		service       corev1.Service
		expectedNodes []*v1.Node
	}{
		{"matchSingle", CSCloud{serviceLabel: "app-pool", nodeLabel: "pool"}, s1, nodes[0:1]},
		{"emptyLabels", CSCloud{}, s1, nodes},
		{"matchMultiple", CSCloud{serviceLabel: "app-pool", nodeLabel: "pool"}, s2, nodes[1:3]},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			filtered := tc.cs.filterNodesMatchingLabels(nodes, tc.service)
			if !reflect.DeepEqual(filtered, tc.expectedNodes) {
				t.Errorf("Expected %+v. Got %+v.", tc.expectedNodes, filtered)
			}
		})
	}
}

func TestCheckLoadBalancerRule(t *testing.T) {
	tests := []struct {
		svcIP        string
		svcPorts     []corev1.ServicePort
		rule         *loadBalancerRule
		existing     bool
		needsUpdate  bool
		deleteCalled bool
		errorMatches string
	}{
		{
			existing:    false,
			needsUpdate: false,
		},
		{
			svcIP: "10.0.0.1",
			svcPorts: []corev1.ServicePort{
				{Port: 1234, NodePort: 4567},
			},
			rule: &loadBalancerRule{
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Publicport:  "1234",
					Privateport: "4567",
					Publicip:    "10.0.0.1",
					Tags: []cloudstack.LoadBalancerRuleTags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:    true,
			needsUpdate: false,
		},
		{
			svcIP: "10.0.0.1",
			svcPorts: []corev1.ServicePort{
				{Port: 1234, NodePort: 4567},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Publicport:  "1234",
					Privateport: "4567",
					Publicip:    "10.0.0.1",
					Tags: []cloudstack.LoadBalancerRuleTags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:    true,
			needsUpdate: false,
		},
		{
			svcIP: "10.0.0.2",
			svcPorts: []corev1.ServicePort{
				{Port: 1234, NodePort: 4567},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Publicport:  "1234",
					Privateport: "4567",
					Publicip:    "10.0.0.1",
					Tags: []cloudstack.LoadBalancerRuleTags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:     false,
			needsUpdate:  false,
			deleteCalled: true,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 5, NodePort: 6},
				{Port: 2, NodePort: 20},
				{Port: 10, NodePort: 5},
				{Port: 3, NodePort: 4},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{
					"3:4",
					"5:6",
					"10:5",
				},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Publicport:  "2",
					Privateport: "20",
					Tags: []cloudstack.LoadBalancerRuleTags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:    true,
			needsUpdate: false,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 5, NodePort: 6},
				{Port: 2, NodePort: 20},
				{Port: 10, NodePort: 5},
				{Port: 3, NodePort: 4},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Publicport:  "2",
					Privateport: "20",
					Tags: []cloudstack.LoadBalancerRuleTags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:     false,
			needsUpdate:  false,
			deleteCalled: true,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 1, NodePort: 2},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Publicport:  "1",
					Privateport: "2",
					Algorithm:   "x",
					Tags: []cloudstack.LoadBalancerRuleTags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:     true,
			needsUpdate:  true,
			deleteCalled: false,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 1, NodePort: 2},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Publicport:  "1",
					Privateport: "2",
					Tags: []cloudstack.LoadBalancerRuleTags{
						{Key: serviceTag}, {Key: cloudProviderTag},
					},
				},
			},
			existing:     true,
			needsUpdate:  true,
			deleteCalled: false,
		},
	}

	var deleteCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Query().Get("command"), "deleteLoadBalancerRule")
		deleteCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	fakeCli := cloudstack.NewAsyncClient(srv.URL, "", "", false)
	cloud := &CSCloud{
		environments: map[string]CSEnvironment{
			"test": CSEnvironment{
				client: fakeCli,
			},
		},
	}
	for i, tt := range tests {
		deleteCalled = false
		lb := loadBalancer{
			name: "test",
			cloud: &projectCloud{
				environment: "test",
				CSCloud:     cloud,
			},
			ip: cloudstackIP{
				address: tt.svcIP,
			},
			rule: tt.rule,
		}
		t.Run(fmt.Sprintf("test %d", i), func(t *testing.T) {
			existing, needsUpdate, err := lb.checkLoadBalancerRule("name", tt.svcPorts)
			if tt.errorMatches != "" {
				assert.Error(t, err)
				assert.Regexp(t, tt.errorMatches, err.Error())
				assert.False(t, existing)
				assert.False(t, needsUpdate)
			} else {
				assert.Equal(t, tt.existing, existing)
				assert.Equal(t, tt.needsUpdate, needsUpdate)
			}
			assert.Equal(t, tt.deleteCalled, deleteCalled)
		})
	}
}
