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
	nodes := []*corev1.Node{
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
	svc := &corev1.Service{
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

	lbStatus, err := csCloud.EnsureLoadBalancer("kubernetes", svc, nodes)
	require.NoError(t, err)
	assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
		Ingress: []corev1.LoadBalancerIngress{
			{IP: "10.0.0.1", Hostname: "svc1.test.com"},
		},
	})

	lbStatus, err = csCloud.EnsureLoadBalancer("kubernetes", svc, nodes)
	require.NoError(t, err)
	assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
		Ingress: []corev1.LoadBalancerIngress{
			{IP: "10.0.0.1", Hostname: "svc1.test.com"},
		},
	})

	srv.HasCalls(t, []cloudstackFake.MockAPICall{
		{Command: "listVirtualMachines"},
		{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
		{Command: "listNetworks"},
		{Command: "associateIpAddress"},
		{Command: "queryAsyncJobResult"},
		{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}}},
		{Command: "queryAsyncJobResult"},
		{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
		{Command: "queryAsyncJobResult"},
		{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
		{Command: "queryAsyncJobResult"},
		{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
		{Command: "queryAsyncJobResult"},
		{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
		{Command: "queryAsyncJobResult"},
		{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
		{Command: "queryAsyncJobResult"},

		// Second EnsureLoadBalancer call
		{Command: "listVirtualMachines"},
		{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
		{Command: "listTags"},
		{Command: "listTags"},
		{Command: "listTags"},
	})
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
			name:        "test",
			environment: "test",
			CSCloud:     cloud,
			ipAddr:      tt.svcIP,
			rule:        tt.rule,
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
