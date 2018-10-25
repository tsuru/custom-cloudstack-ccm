package cloudstack

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xanzy/go-cloudstack/cloudstack"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFilterNodesMatchingLabels(t *testing.T) {
	nodes := []*v1.Node{
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "pool1"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "pool2"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "pool2"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}},
	}
	s1 := v1.Service{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app-pool": "pool1"}}}
	s2 := v1.Service{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app-pool": "pool2"}}}
	tt := []struct {
		name          string
		cs            CSCloud
		service       v1.Service
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
		svcPorts     []v1.ServicePort
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
			svcPorts: []v1.ServicePort{
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
			svcPorts: []v1.ServicePort{
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
			svcPorts: []v1.ServicePort{
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
			svcPorts: []v1.ServicePort{
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
			svcPorts: []v1.ServicePort{
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
			svcPorts: []v1.ServicePort{
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
			svcPorts: []v1.ServicePort{
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
