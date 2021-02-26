package cloudstack

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cloudstackFake "github.com/tsuru/custom-cloudstack-ccm/cloudstack/fake"
	"github.com/xanzy/go-cloudstack/v2/cloudstack"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeFake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	kubeTesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
)

func init() {
	flag.Set("v", "10")
	flag.Set("logtostderr", "true")
	defaultFailureBackoff = 200 * time.Millisecond
}

var globalTestEvents struct {
	sync.Mutex
	events []string
}

func newTestCSCloud(t *testing.T, cfg *CSConfig, kubeClient *kubeFake.Clientset) *CSCloud {
	csCloud, err := newCSCloud(cfg)
	require.Nil(t, err)
	if kubeClient != nil {
		csCloud.kubeClient = kubeClient
	} else {
		csCloud.kubeClient = kubeFake.NewSimpleClientset()
	}

	globalTestEvents.Lock()
	globalTestEvents.events = nil
	globalTestEvents.Unlock()

	b := record.NewBroadcaster()
	b.StartLogging(func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		fmt.Printf("EVENT: %s\n", msg)
		globalTestEvents.Lock()
		globalTestEvents.events = append(globalTestEvents.events, msg)
		globalTestEvents.Unlock()
	})
	csCloud.recorder = b.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "csccm"})

	return csCloud
}

func waitEvent(t *testing.T, expected string) {
	timeout := time.After(2 * time.Second)
	for {
		select {
		case <-timeout:
			t.Errorf("timeout waiting for event with message %q", expected)
			return
		case <-time.After(100 * time.Millisecond):
		}
		globalTestEvents.Lock()
		evts := globalTestEvents.events
		if len(evts) > 0 {
			if strings.Contains(evts[len(evts)-1], expected) {
				globalTestEvents.Unlock()
				return
			}
		}
		globalTestEvents.Unlock()
	}
}

func Test_CSCloud_EnsureLoadBalancer(t *testing.T) {
	baseNodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "n1",
				Labels: map[string]string{
					"my/project-label":  "11111111-2222-3333-4444-555555555555",
					"environment-label": "env1",
					"pool-label":        "pool1",
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
				"pool-label":        "pool1",
			},
			Annotations: map[string]string{},
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

	baseAssert := func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
		require.NoError(t, err)
		assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{
				{IP: "10.0.0.1", Hostname: "svc1.test.com"},
			},
		})
		srv.HasCalls(t, []cloudstackFake.MockAPICall{
			{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n1"}, "projectid": []string{"11111111-2222-3333-4444-555555555555"}}},
			{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
			{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
			{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
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
			{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
			{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
			{Command: "queryAsyncJobResult"},
			{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
			{Command: "queryAsyncJobResult"},
		})
	}

	assertSvcWithProject := func(t *testing.T, svc *corev1.Service) {
		assert.Equal(t, "11111111-2222-3333-4444-555555555555", svc.Labels["my/project-label"])
	}

	type consecutiveCall struct {
		svc       corev1.Service
		nodes     []*corev1.Node
		assert    func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error)
		assertSvc func(t *testing.T, svc *corev1.Service)
	}

	tests := []struct {
		name    string
		prepend *kubeFake.Clientset
		hook    func(t *testing.T, srv *cloudstackFake.CloudstackServer)
		calls   []consecutiveCall
	}{
		{
			name: "basic ensure",
			calls: []consecutiveCall{
				{
					svc:       baseSvc,
					assert:    baseAssert,
					assertSvc: assertSvcWithProject,
				},
			},
		},

		{
			name: "service with no ports fails",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.Ports = nil
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `requested load balancer with no ports`)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{})
					},
				},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},

		{
			name: "second ensure with empty nodes fails",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc:   baseSvc,
					nodes: []*corev1.Node{},
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `no nodes available to add to load balancer`)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{})
					},
				},
			},
		},

		{
			name: "second ensure with new node adds it",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n2 := baseNodes[0].DeepCopy()
						n2.Name = "n2"
						return []*corev1.Node{
							baseNodes[0].DeepCopy(),
							n2,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n2"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net2"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm2"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
			},
		},

		{
			name: "second ensure with nodes in different pool does nothing",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n2 := baseNodes[0].DeepCopy()
						n2.Labels["pool-label"] = "pool-other"
						n2.Name = "n2"
						return []*corev1.Node{
							baseNodes[0].DeepCopy(),
							n2,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n2"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},

		{
			name: "every node in different pool fails",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n1 := baseNodes[0].DeepCopy()
						n1.Labels["pool-label"] = "pool-other"
						return []*corev1.Node{
							n1,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `no nodes available to add to service myns/svc1`)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{})
					},
				},
			},
		},

		{
			name: "every node with no matching vms fails",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n1 := baseNodes[0].DeepCopy()
						n1.Name = "notfound"
						return []*corev1.Node{
							n1,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `unable to map kubernetes nodes to cloudstack instances for nodes: notfound`)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"notfound"}}},
						})
					},
				},
			},
		},

		{
			name: "removes old nodes with valid new nodes",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n2 := baseNodes[0].DeepCopy()
						n2.Name = "n2"
						return []*corev1.Node{
							n2,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n2"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net2"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm2"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "removeFromLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-2"}}},
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
			name: "add extra params for associateIPAddress",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Labels["csccm.cloudprovider.io/associateipaddress-extra-param-foo"] = "bar"
						svc.Annotations["csccm.cloudprovider.io/associateipaddress-extra-param-lbenvironmentid"] = "3"
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"foo": []string{"bar"}, "lbenvironmentid": []string{"3"}, "networkid": []string{"net1"}}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
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
			name: "add extra params for createLoadBalancerRule",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/createloadbalancer-extra-param-dsr"] = "true"
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"lbenvironmentid": []string{"1"}, "networkid": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"dsr": []string{"true"}, "name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
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
			name: "create load balancer and modify health check for all created load balancer pool, but fail at the first attempt",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				calls := 0
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "assignToLoadBalancerRule" && calls == 0 {
						srv.SetDefaultLBPoolCreation(r.FormValue("id"))
						calls++
					}
					if cmd == "listGloboNetworkPools" && calls == 1 {
						calls++
						return false
					}
					if cmd == "listGloboNetworkPools" && calls == 2 {
						srv.SetDefaultLBPoolCreation(r.FormValue("lbruleid"))
						calls++
					}
					return false
				}
			},
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck"] = "true"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-http"] = "GET / HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-https-bar"] = "GET /test HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-http"] = "200 OK"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-https-bar"] = "bleh"
						svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolTCP},
							{Name: "https-bar", Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolTCP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						waitEvent(t, "error list load balancer pools for lb(svc1.test.com, 11111111-2222-3333-4444-555555555555, lbrule(lbrule-1, svc1.test.com), ip(ip-1, 10.0.0.1), svc(myns/svc1)): no LB pools found")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"lbenvironmentid": []string{"1"}, "networkid": []string{"net1"}}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
						})
						// wait for failed event backoff
						time.Sleep(2 * time.Second)
					},
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck"] = "true"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-http"] = "GET / HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-https-bar"] = "GET /test HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-http"] = "200 OK"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-https-bar"] = "bleh"
						svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolTCP},
							{Name: "https-bar", Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolTCP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"0"}, "healthchecktype": []string{"HTTP"}, "healthcheck": []string{"GET / HTTP/1.0"}, "expectedhealthcheck": []string{"200 OK"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"1"}, "healthchecktype": []string{"HTTPS"}, "healthcheck": []string{"GET /test HTTP/1.0"}, "expectedhealthcheck": []string{"bleh"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},
		{
			name: "create load balancer and modify health for just one load balancer pool",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck"] = "true"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-https-foo"] = "GET /test HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-https-foo"] = "200 OK"
						svc.Spec.Ports = []corev1.ServicePort{{Name: "http-foo", Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolTCP},
							{Name: "https-foo", Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolTCP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"lbenvironmentid": []string{"1"}, "networkid": []string{"net1"}}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"1"}, "healthchecktype": []string{"HTTPS"}, "healthcheck": []string{"GET /test HTTP/1.0"}, "expectedhealthcheck": []string{"200 OK"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
			},
		},
		{
			name: "create load balancer and try modify healthcheck using target port instead node port",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck"] = "true"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-https-foo"] = "GET /test HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-https-foo"] = "200 OK"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-use-targetport"] = "true"
						svc.Spec.Ports = []corev1.ServicePort{{Name: "http-foo", Port: 8080, NodePort: 30001, TargetPort: intstr.IntOrString{IntVal: 8080}, Protocol: corev1.ProtocolTCP},
							{Name: "https-foo", Port: 8443, NodePort: 30002, TargetPort: intstr.IntOrString{IntVal: 8443}, Protocol: corev1.ProtocolTCP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"lbenvironmentid": []string{"1"}, "networkid": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"8080"}, "additionalportmap": []string{"8443:8443"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"1"}, "healthchecktype": []string{"HTTPS"}, "healthcheck": []string{"GET /test HTTP/1.0"}, "expectedhealthcheck": []string{"200 OK"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
			},
		},
		{
			name: "create load balancer and try modify healthcheck using target port using string instead int",
			prepend: kubeFake.NewSimpleClientset(&corev1.EndpointsList{
				Items: []corev1.Endpoints{
					{ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "myns",
					},
						Subsets: []corev1.EndpointSubset{{Ports: []corev1.EndpointPort{{Name: "http", Port: 8080}, {Name: "https-foo", Port: 8443}}}}},
				}}),
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck"] = "true"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-https-foo"] = "GET /test HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-https-foo"] = "200 OK"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-use-targetport"] = "true"
						svc.Spec.Ports = []corev1.ServicePort{{Name: "http-foo", Port: 8080, NodePort: 30001, TargetPort: intstr.IntOrString{IntVal: 8080}, Protocol: corev1.ProtocolTCP},
							{Name: "https-foo", Port: 8443, NodePort: 30002, TargetPort: intstr.FromString("https-foo"), Protocol: corev1.ProtocolTCP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"lbenvironmentid": []string{"1"}, "networkid": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"8080"}, "additionalportmap": []string{"8443:8443"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"1"}, "healthchecktype": []string{"HTTPS"}, "healthcheck": []string{"GET /test HTTP/1.0"}, "expectedhealthcheck": []string{"200 OK"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
			},
		},
		{
			name: "update already created load balancer and modify health check for load balancer pools only once",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.Ports = []corev1.ServicePort{{Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolTCP},
							{Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolTCP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress"},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "publicport": []string{"8080"}, "additionalportmap": []string{"8443:30002"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck"] = "true"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-http-foo"] = "GET / HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-http-foo"] = "200 OK"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-https-bar"] = "GET /test HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-https-bar"] = "bleh"
						svc.Spec.Ports = []corev1.ServicePort{{Name: "http-foo", Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolTCP},
							{Name: "https-bar", Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolTCP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"0"}, "healthchecktype": []string{"HTTP"}, "healthcheck": []string{"GET / HTTP/1.0"}, "expectedhealthcheck": []string{"200 OK"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"1"}, "healthchecktype": []string{"HTTPS"}, "healthcheck": []string{"GET /test HTTP/1.0"}, "expectedhealthcheck": []string{"bleh"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck"] = "true"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-http-foo"] = "GET / HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-http-foo"] = "200 OK"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-msg-https-bar"] = "GET /test HTTP/1.0"
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-custom-healthcheck-rsp-https-bar"] = "bleh"
						svc.Spec.Ports = []corev1.ServicePort{{Name: "http-foo", Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolTCP},
							{Name: "https-bar", Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolTCP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},
		{
			name: "second ensure using custom load balancer name",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Labels["csccm.cloudprovider.io/loadbalancer-name"] = "foo.bar.com"
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "foo.bar.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"foo.bar.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "deleteLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"foo.bar.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-2"}}},
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
			name: "second ensure using custom suffix for load balancer name",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Labels["csccm.cloudprovider.io/loadbalancer-name-suffix"] = "anotherrealm.com"
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.anotherrealm.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.anotherrealm.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "deleteLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.anotherrealm.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-2"}}},
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
			name: "set use-targetport annotation creates LB using targetport same as destination",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-use-targetport"] = "true"
						svc.Spec.Ports = []corev1.ServicePort{{Port: 80, NodePort: 30002, TargetPort: intstr.IntOrString{IntVal: 80}, Protocol: corev1.ProtocolTCP}, {Port: 443, NodePort: 30003, TargetPort: intstr.IntOrString{IntVal: 443}, Protocol: corev1.ProtocolTCP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"lbenvironmentid": []string{"1"}, "networkid": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"80"}, "additionalportmap": []string{"443:443"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
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
			name: "updating ports while returned multiple lb rules with same suffix lbname",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				srv.AddLBRule("foo.svc1.test.com", cloudstackFake.LoadBalancerRule{Rule: map[string]interface{}{
					"id":         "foo.svc1.test.com",
					"name":       "foo.svc1.test.com",
					"publicip":   "10.0.0.2",
					"publicipid": "foo.svc1.test.com",
					"networkid":  "1234",
				}})
				srv.AddLBRule("bar.svc1.test.com", cloudstackFake.LoadBalancerRule{Rule: map[string]interface{}{
					"id":         "bar.svc1.test.com",
					"name":       "bar.svc1.test.com",
					"publicip":   "10.0.0.3",
					"publicipid": "bar.svc1.test.com",
					"networkid":  "5678",
				}})
			},
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.Ports[0].NodePort = 30003
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "deleteLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30003"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-2"}}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.Error(t, err)
						require.Contains(t, err.Error(), "my error")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"page": []string{"1"}}},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						waitEvent(t, "my error")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
						})
						// wait for failed task backoff
						time.Sleep(2 * time.Second)
					},
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "updateLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "algorithm": []string{"source"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},

		{
			name: "allocate ip tag error",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				calls := 0
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "createTags" && r.FormValue("resourceids") == "ip-1" && calls == 0 {
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.Error(t, err)
						assert.Contains(t, err.Error(), "my error")
						assert.NotContains(t, err.Error(), "error rolling back")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses"},
							{Command: "listNetworks"},
							{Command: "associateIpAddress"},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "disassociateIpAddress"},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.2", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress"},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-2"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-2"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-2"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-2"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
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
			name: "service requesting explicit IP, ip not found",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.LoadBalancerIP = "192.168.9.9"
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.Error(t, err)
						assert.Contains(t, err.Error(), "could not find IP address 192.168.9.9")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"page": []string{"1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"ipaddress": []string{"192.168.9.9"}}},
						})
					},
				},
			},
		},

		{
			name: "service requesting explicit IP, ip found",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				srv.AddIP(cloudstack.PublicIpAddress{
					Id:        "mycustomip",
					Ipaddress: "192.168.9.9",
				})
			},
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.LoadBalancerIP = "192.168.9.9"
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "192.168.9.9", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"page": []string{"1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"ipaddress": []string{"192.168.9.9"}}},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"mycustomip"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
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
			name: "existing load balancer missing namespace tag",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				calls := 0
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					matching := r.FormValue("command") == "createTags" &&
						r.FormValue("resourceids") == "lbrule-1" &&
						r.FormValue("tags[0].key") == "kubernetes_namespace" &&
						calls == 0
					if matching {
						calls++
						obj := cloudstack.CreateTagsResponse{
							JobID:   "myjobid",
							Success: true,
						}
						w.Write(cloudstackFake.MarshalResponse("createTagsResponse", obj))
						srv.Jobs["myjobid"] = func() interface{} {
							return obj
						}
						return true
					}
					return false
				}
			},
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},

		{
			name: "existing service with LB requesting change to explicit IP",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				srv.AddIP(cloudstack.PublicIpAddress{
					Id:        "mycustomip",
					Ipaddress: "192.168.9.9",
				})
			},
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.LoadBalancerIP = "192.168.9.9"
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "192.168.9.9", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"ipaddress": []string{"192.168.9.9"}}},
							{Command: "deleteLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listPublicIpAddresses", Params: url.Values{"id": []string{"ip-1"}}},
							{Command: "disassociateIpAddress", Params: url.Values{"id": []string{"ip-1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"mycustomip"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-2"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-2"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-2"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-2"}}},
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
			name: "existing service with LB requesting change to explicit IP, IP not found error",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.LoadBalancerIP = "192.168.9.9"
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.Error(t, err)
						assert.Contains(t, err.Error(), "could not find IP address 192.168.9.9")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"ipaddress": []string{"192.168.9.9"}}},
						})
					},
				},
			},
		},

		{
			name: "existing service with no LB rule and existing IP with no tags",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				srv.AddIP(cloudstack.PublicIpAddress{
					Id:        "mycustomip",
					Ipaddress: "192.168.9.9",
				})
			},
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
							{IP: "192.168.9.9"},
						}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "192.168.9.9", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"page": []string{"1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"ipaddress": []string{"192.168.9.9"}}},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"mycustomip"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"mycustomip"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"mycustomip"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"mycustomip"}, "privateport": []string{"30001"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
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
			name: "fails for LB without required tags with no status",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				srv.AddLBRule("svc1.test.gom", cloudstackFake.LoadBalancerRule{Rule: map[string]interface{}{
					"id":         "lbrule-1",
					"name":       "svc1.test.com",
					"publicip":   "10.0.0.1",
					"publicipid": "ip-1",
					"networkid":  "1234",
				}})
			},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `should not manage lb(svc1.test.com, 11111111-2222-3333-4444-555555555555, lbrule(lbrule-1, svc1.test.com), ip(ip-1, 10.0.0.1), svc(myns/svc1)) - status hostname: "" - missing tags: tag "cloudprovider": expected: "custom-cloudstack", got: "" - tag "kubernetes_service": expected: "svc1", got: ""`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
						})
					},
				},
			},
		},

		{
			name: "uses status hostname information if no tags exist",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				calls := 0
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "listLoadBalancerRules" {
						if calls < 2 {
							calls++
							return false
						}
						srv.DeleteTags("lbrule-1", []string{"cloudprovider"})
					}
					return false
				}
			},
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},

		{
			name: "fails with list LB error",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "listLoadBalancerRules" {
						w.WriteHeader(http.StatusInternalServerError)
						w.Write(cloudstackFake.ErrorResponse(cmd+"Response", "myerror"))
						return true
					}
					return false
				}
			},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `load balancer svc1.test.com for service myns/svc1 get rule error: CloudStack API error 999 (CSExceptionErrorCode: 999): myerror`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
						})
					},
				},
			},
		},

		{
			name: "fails with invalid session affinity",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.SessionAffinity = corev1.ServiceAffinity("invalid")
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `unsupported load balancer affinity: invalid`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
						})
					},
				},
			},
		},

		{
			name: "fails if service patch fails",
			prepend: (func() *kubeFake.Clientset {
				cli := kubeFake.NewSimpleClientset()
				cli.PrependReactor("patch", "services", func(action kubeTesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("my patch error")
				})
				return cli
			})(),
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `unable to patch service with project-id label: my patch error`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
						})
					},
				},
			},
		},

		{
			name: "fails with target port that doesn't exist on new LB",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-use-targetport"] = "true"
						svc.Spec.Ports = []corev1.ServicePort{
							{Name: "http-foo", Port: 8080, NodePort: 30001, TargetPort: intstr.IntOrString{IntVal: 8080}, Protocol: corev1.ProtocolTCP},
							{Name: "https-foo", Port: 8443, NodePort: 30002, TargetPort: intstr.FromString("https-foo"), Protocol: corev1.ProtocolTCP},
						}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `error get endpoints: endpoints "svc1" not found`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"lbenvironmentid": []string{"1"}, "networkid": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"ip-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
			},
		},

		{
			name: "fails with target port that doesn't exist on existing LB",
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-use-targetport"] = "true"
						svc.Spec.Ports = []corev1.ServicePort{
							{Name: "http-foo", Port: 8080, NodePort: 30001, TargetPort: intstr.IntOrString{IntVal: 8080}, Protocol: corev1.ProtocolTCP},
							{Name: "https-foo", Port: 8443, NodePort: 30002, TargetPort: intstr.FromString("https-foo"), Protocol: corev1.ProtocolTCP},
						}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `error get endpoints: endpoints "svc1" not found`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
						})
					},
				},
			},
		},

		{
			name: "fails on update error",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "updateLoadBalancerRule" {
						w.WriteHeader(http.StatusInternalServerError)
						w.Write(cloudstackFake.ErrorResponse(cmd+"Response", "myerror"))
						return true
					}
					return false
				}
			},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.EqualError(t, err, `unable to update load balancer lb(svc1.test.com, 11111111-2222-3333-4444-555555555555, lbrule(lbrule-1, svc1.test.com), ip(ip-1, 10.0.0.1), svc(myns/svc1)): CloudStack API error 999 (CSExceptionErrorCode: 999): myerror`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "updateLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "algorithm": []string{"source"}}},
						})
					},
				},
			},
		},

		{
			name: "fails on assign tags to rule",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				calls := 0
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "createTags" && r.FormValue("resourceids") == "lbrule-1" && calls == 0 {
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `error adding tags to LoadBalancer lbrule-1: CloudStack API error 999 (CSExceptionErrorCode: 999): my error`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n1"}, "projectid": []string{"11111111-2222-3333-4444-555555555555"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
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
						})
					},
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `should not manage lb(svc1.test.com, 11111111-2222-3333-4444-555555555555, lbrule(lbrule-1, svc1.test.com), ip(ip-1, 10.0.0.1), svc(myns/svc1)) - status hostname: "" - missing tags: tag "cloudprovider": expected: "custom-cloudstack", got: "" - tag "kubernetes_service": expected: "svc1", got: ""`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
						})
					},
				},
			},
		},

		{
			name: "retry tag assign after error on update",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				calls := 0
				listCalls := 0
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "listLoadBalancerRules" {
						listCalls++
						if listCalls == 3 {
							srv.DeleteTags("lbrule-1", []string{"cloudprovider"})
						}
						return false
					}
					if cmd == "createTags" && r.FormValue("resourceids") == "lbrule-1" {
						calls++
						if calls == 4 {
							w.WriteHeader(http.StatusInternalServerError)
							w.Write(cloudstackFake.ErrorResponse(cmd+"Response", "my error"))
							return true
						}
					}
					return false
				}
			},
			calls: []consecutiveCall{
				{
					svc:    baseSvc,
					assert: baseAssert,
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `error adding tags to LoadBalancer lbrule-1: CloudStack API error 999 (CSExceptionErrorCode: 999): my error`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
						})
					},
				},
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"resourceids": []string{"lbrule-1"}, "tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},

		{
			name: "fails with ignore tag set",
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				srv.AddLBRule("svc1.test.gom", cloudstackFake.LoadBalancerRule{Rule: map[string]interface{}{
					"id":         "lbrule-1",
					"name":       "svc1.test.com",
					"publicip":   "10.0.0.1",
					"publicipid": "ip-1",
					"networkid":  "1234",
				}})
				srv.AddIP(cloudstack.PublicIpAddress{
					Id:        "ip-1",
					Ipaddress: "10.0.0.1",
				})
				srv.AddTags("lbrule-1", []cloudstack.Tags{
					{Key: "cloudprovider-ignore", Value: "1"},
					{Key: "cloudprovider", Value: "custom-cloudstack"},
					{Key: "kubernetes_service", Value: "svc1"},
					{Key: "kubernetes_namespace", Value: "myns"},
				})
			},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						assert.EqualError(t, err, `should not manage lb(svc1.test.com, 11111111-2222-3333-4444-555555555555, lbrule(lbrule-1, svc1.test.com), ip(ip-1, 10.0.0.1), svc(myns/svc1)), tag "cloudprovider-ignore" is set`)
						assert.Nil(t, lbStatus)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n1"}, "projectid": []string{"11111111-2222-3333-4444-555555555555"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
						})
					},
				},
			},
		},
		{
			name: "udp load balancer",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.Ports[0].Protocol = corev1.ProtocolUDP
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"lbenvironmentid": []string{"1"}, "networkid": []string{"net1"}}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"0"}, "healthchecktype": []string{"UDP"}, "healthcheck": []string{""}, "expectedhealthcheck": []string{""}, "l4protocol": []string{"UDP"}, "l7protocol": []string{"Outros"}, "redeploy": []string{"true"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.Ports[0].Protocol = corev1.ProtocolUDP
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},
		{
			name: "udp load balancer with multiple ports",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.Ports = []corev1.ServicePort{{Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolUDP},
							{Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolUDP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listNetworks"},
							{Command: "associateIpAddress", Params: url.Values{"lbenvironmentid": []string{"1"}, "networkid": []string{"net1"}}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"0"}, "healthchecktype": []string{"UDP"}, "healthcheck": []string{""}, "expectedhealthcheck": []string{""}, "l4protocol": []string{"UDP"}, "l7protocol": []string{"Outros"}, "redeploy": []string{"true"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"1"}, "healthchecktype": []string{"UDP"}, "healthcheck": []string{""}, "expectedhealthcheck": []string{""}, "l4protocol": []string{"UDP"}, "l7protocol": []string{"Outros"}, "redeploy": []string{"true"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.Ports = []corev1.ServicePort{{Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolUDP},
							{Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolUDP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},
		{
			name: "mixed ports tcp and udp",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.Ports = []corev1.ServicePort{{Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolTCP},
							{Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolUDP}}
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, err error) {
						require.EqualError(t, err, `unsupported load balancer with multiple protocols: "TCP" and "UDP"`)
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := cloudstackFake.NewCloudstackServer()
			defer srv.Close()
			csCloud := newTestCSCloud(t, &CSConfig{
				Global: globalConfig{
					EnvironmentLabel:   "environment-label",
					ProjectIDLabel:     "my/project-label",
					NodeFilterLabel:    "pool-label",
					ServiceFilterLabel: "pool-label",
				},
				Command: commandConfig{
					AssignNetworks: "assignNetworkToLBRule",
				},
				Environment: map[string]*environmentConfig{
					"env1": {
						APIURL:          srv.URL,
						APIKey:          "a",
						SecretKey:       "b",
						LBEnvironmentID: "1",
						LBDomain:        "test.com",
					},
				},
			}, tt.prepend)
			if tt.hook != nil {
				tt.hook(t, srv)
			}
			var err error
			var persistentLBStatus *corev1.LoadBalancerStatus
			for i, cc := range tt.calls {
				t.Logf("call %d", i)

				csCloud.updateLBQueue.start(context.Background())

				svc := cc.svc.DeepCopy()
				if err == nil && persistentLBStatus != nil {
					svc.Status.LoadBalancer = *persistentLBStatus
				}
				if i == 0 {
					_, err = csCloud.kubeClient.CoreV1().Services(svc.Namespace).Create(svc)
				} else {
					_, err = csCloud.kubeClient.CoreV1().Services(svc.Namespace).Update(svc)
				}
				assert.NoError(t, err)
				nodes := cc.nodes
				if nodes == nil {
					nodes = baseNodes
				}
				lbStatus, err := csCloud.EnsureLoadBalancer(context.Background(), "kubernetes", svc, nodes)
				if err == nil {
					persistentLBStatus = lbStatus
				}

				csCloud.updateLBQueue.stopWait()

				if cc.assert != nil {
					cc.assert(t, srv, lbStatus, err)
				}
				if cc.assertSvc != nil {
					clusterSvc, err := csCloud.kubeClient.CoreV1().Services(svc.Namespace).Get(svc.Name, metav1.GetOptions{})
					assert.NoError(t, err)
					cc.assertSvc(t, clusterSvc)
				}
				srv.Calls = nil

				// flush pending calls
				csCloud.updateLBQueue.start(context.Background())
				csCloud.updateLBQueue.stopWait()
			}
		})
	}
}

func Test_CSCloud_GetLoadBalancer(t *testing.T) {
	baseNodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "n1",
				Labels: map[string]string{
					"project-label":     "11111111-2222-3333-4444-555555555555",
					"environment-label": "env1",
					"pool-label":        "pool1",
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
				"pool-label":        "pool1",
			},
			Annotations: map[string]string{},
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

	tests := []struct {
		name        string
		svc         corev1.Service
		hook        func(t *testing.T, srv *cloudstackFake.CloudstackServer)
		assert      func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, exists bool, err error)
		ensureNodes []*corev1.Node
	}{
		{
			name: "load balancer not found",
			svc:  baseSvc,
			assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, exists bool, err error) {
				assert.NoError(t, err)
				assert.Equal(t, exists, false)
				assert.Nil(t, lbStatus)
				srv.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
					{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
				})
			},
		},
		{
			name:        "load balancer found",
			svc:         baseSvc,
			ensureNodes: baseNodes,
			assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, exists bool, err error) {
				assert.NoError(t, err)
				assert.Equal(t, exists, true)
				assert.Equal(t, &corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{
						{Hostname: "svc1.test.com", IP: "10.0.0.1"},
					},
				}, lbStatus)
				srv.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}, "projectid": []string{"11111111-2222-3333-4444-555555555555"}}},
				})
			},
		},
		{
			name: "fails with list LB error",
			svc:  baseSvc,
			hook: func(t *testing.T, srv *cloudstackFake.CloudstackServer) {
				srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
					cmd := r.FormValue("command")
					if cmd == "listLoadBalancerRules" {
						w.WriteHeader(http.StatusInternalServerError)
						w.Write(cloudstackFake.ErrorResponse(cmd+"Response", "myerror"))
						return true
					}
					return false
				}
			},
			assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *corev1.LoadBalancerStatus, exists bool, err error) {
				assert.EqualError(t, err, `load balancer svc1.test.com for service myns/svc1 get rule error: CloudStack API error 999 (CSExceptionErrorCode: 999): myerror`)
				assert.Equal(t, exists, false)
				assert.Nil(t, lbStatus)
				srv.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := cloudstackFake.NewCloudstackServer()
			defer srv.Close()
			if tt.hook != nil {
				tt.hook(t, srv)
			}
			csCloud := newTestCSCloud(t, &CSConfig{
				Global: globalConfig{
					EnvironmentLabel:   "environment-label",
					ProjectIDLabel:     "project-label",
					NodeFilterLabel:    "pool-label",
					ServiceFilterLabel: "pool-label",
				},
				Command: commandConfig{
					AssignNetworks: "assignNetworkToLBRule",
				},
				Environment: map[string]*environmentConfig{
					"env1": {
						APIURL:          srv.URL,
						APIKey:          "a",
						SecretKey:       "b",
						LBEnvironmentID: "1",
						LBDomain:        "test.com",
					},
				},
			}, nil)
			var err error
			if tt.ensureNodes != nil {
				_, err = csCloud.kubeClient.CoreV1().Services(tt.svc.Namespace).Create(&tt.svc)
				assert.NoError(t, err)
				csCloud.updateLBQueue.start(context.Background())
				_, err = csCloud.EnsureLoadBalancer(context.Background(), "kuberentes", &tt.svc, tt.ensureNodes)
				assert.NoError(t, err)
				csCloud.updateLBQueue.stopWait()
			}
			srv.Calls = nil
			lb, exists, err := csCloud.GetLoadBalancer(context.Background(), "kubernetes", &tt.svc)
			tt.assert(t, srv, lb, exists, err)
		})
	}
}

func Test_CSCloud_UpdateLoadBalancer(t *testing.T) {
	baseNodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "n1",
				Labels: map[string]string{
					"project-label":     "11111111-2222-3333-4444-555555555555",
					"environment-label": "env1",
					"pool-label":        "pool1",
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
				"pool-label":        "pool1",
			},
			Annotations: map[string]string{},
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

	type consecutiveCall struct {
		svc    corev1.Service
		nodes  []*corev1.Node
		assert func(t *testing.T, srv *cloudstackFake.CloudstackServer, err error)
	}

	tests := []struct {
		name       string
		prepend    *kubeFake.Clientset
		hook       func(t *testing.T, srv *cloudstackFake.CloudstackServer)
		calls      []consecutiveCall
		ensureCall *consecutiveCall
	}{
		{
			name: "load balancer not found",
			calls: []consecutiveCall{
				{
					svc:   baseSvc,
					nodes: baseNodes,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n1"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
						})
					},
				},
			},
		},
		{
			name:       "does nothing with existing nodes",
			ensureCall: &consecutiveCall{svc: baseSvc, nodes: baseNodes},
			calls: []consecutiveCall{
				{
					svc:   baseSvc,
					nodes: baseNodes,
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},

		{
			name:       "fails with empty nodes",
			ensureCall: &consecutiveCall{svc: baseSvc, nodes: baseNodes},
			calls: []consecutiveCall{
				{
					svc:   baseSvc,
					nodes: []*corev1.Node{},
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, err error) {
						assert.EqualError(t, err, `no nodes available to add to load balancer`)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{})
					},
				},
			},
		},

		{
			name:       "adds new node",
			ensureCall: &consecutiveCall{svc: baseSvc, nodes: baseNodes},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n2 := baseNodes[0].DeepCopy()
						n2.Name = "n2"
						return []*corev1.Node{
							baseNodes[0].DeepCopy(),
							n2,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n2"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net2"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm2"}}},
							{Command: "queryAsyncJobResult"},
						})
					},
				},
			},
		},

		{
			name:       "does nothing for nodes in different pools",
			ensureCall: &consecutiveCall{svc: baseSvc, nodes: baseNodes},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n2 := baseNodes[0].DeepCopy()
						n2.Labels["pool-label"] = "pool-other"
						n2.Name = "n2"
						return []*corev1.Node{
							baseNodes[0].DeepCopy(),
							n2,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n2"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
						})
					},
				},
			},
		},

		{
			name:       "fails if every node is in a different pool",
			ensureCall: &consecutiveCall{svc: baseSvc, nodes: baseNodes},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n1 := baseNodes[0].DeepCopy()
						n1.Labels["pool-label"] = "pool-other"
						return []*corev1.Node{
							n1,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, err error) {
						assert.EqualError(t, err, `no nodes available to add to service myns/svc1`)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{})
					},
				},
			},
		},

		{
			name:       "fails if every node ha no matching vms",
			ensureCall: &consecutiveCall{svc: baseSvc, nodes: baseNodes},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n1 := baseNodes[0].DeepCopy()
						n1.Name = "notfound"
						return []*corev1.Node{
							n1,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, err error) {
						assert.EqualError(t, err, `unable to map kubernetes nodes to cloudstack instances for nodes: notfound`)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"notfound"}}},
						})
					},
				},
			},
		},

		{
			name:       "removes old nodes when receiving valid new nodes",
			ensureCall: &consecutiveCall{svc: baseSvc, nodes: baseNodes},
			calls: []consecutiveCall{
				{
					svc: baseSvc,
					nodes: (func() []*corev1.Node {
						n2 := baseNodes[0].DeepCopy()
						n2.Name = "n2"
						return []*corev1.Node{
							n2,
						}
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n2"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"1"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net2"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm2"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "removeFromLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
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
			csCloud := newTestCSCloud(t, &CSConfig{
				Global: globalConfig{
					EnvironmentLabel:   "environment-label",
					ProjectIDLabel:     "project-label",
					NodeFilterLabel:    "pool-label",
					ServiceFilterLabel: "pool-label",
				},
				Command: commandConfig{
					AssignNetworks: "assignNetworkToLBRule",
				},
				Environment: map[string]*environmentConfig{
					"env1": {
						APIURL:          srv.URL,
						APIKey:          "a",
						SecretKey:       "b",
						LBEnvironmentID: "1",
						LBDomain:        "test.com",
					},
				},
			}, tt.prepend)
			if tt.hook != nil {
				tt.hook(t, srv)
			}
			var err error
			var lbStatus *corev1.LoadBalancerStatus
			if tt.ensureCall != nil {
				_, err = csCloud.kubeClient.CoreV1().Services(tt.ensureCall.svc.Namespace).Create(&tt.ensureCall.svc)
				assert.NoError(t, err)
				csCloud.updateLBQueue.start(context.Background())
				_, err = csCloud.EnsureLoadBalancer(context.Background(), "kuberentes", &tt.ensureCall.svc, tt.ensureCall.nodes)
				assert.NoError(t, err)
				csCloud.updateLBQueue.stopWait()
			}
			srv.Calls = nil
			for _, env := range csCloud.environments {
				err = env.manager.initCache()
				assert.NoError(t, err)
			}
			for i, cc := range tt.calls {
				csCloud.updateLBQueue.start(context.Background())

				t.Logf("call %d", i)
				svc := cc.svc.DeepCopy()
				if err == nil && lbStatus != nil {
					svc.Status.LoadBalancer = *lbStatus
				}
				err = csCloud.UpdateLoadBalancer(context.Background(), "kubernetes", svc, cc.nodes)

				csCloud.updateLBQueue.stopWait()

				if cc.assert != nil {
					cc.assert(t, srv, err)
				}
				srv.Calls = nil
			}
		})
	}
}

func Test_CSCloud_EnsureLoadBalancerDeleted(t *testing.T) {
	baseSvc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc1",
			Namespace: corev1.NamespaceDefault,
			Annotations: map[string]string{
				"project-label":     "project-1",
				"environment-label": "env1",
			},
		},
	}
	tests := []struct {
		name   string
		svc    *corev1.Service
		setup  func(*cloudstackFake.CloudstackServer)
		assert func(*testing.T, error, *cloudstackFake.CloudstackServer)
	}{
		{
			name: "service is not provided",
			assert: func(t *testing.T, err error, cs *cloudstackFake.CloudstackServer) {
				require.Error(t, err)
				assert.EqualError(t, err, "EnsureLoadBalancerDeleted: service cannot be nil")
			},
		},
		{
			name: "lb rule not found",
			svc: func() *corev1.Service {
				svc := baseSvc.DeepCopy()
				svc.Annotations["environment-label"] = "env2"
				return svc
			}(),
			assert: func(t *testing.T, err error, cs *cloudstackFake.CloudstackServer) {
				require.NoError(t, err)
				cs.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.env2.test.com"}, "projectid": []string{"project-1"}, "listall": []string{"true"}}},
					{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}, "projectid": []string{"project-1"}, "listall": []string{"true"}}},
				})
			},
		},
		{
			name: "lb removal config either on service labels or environment configs is not enabled",
			svc:  baseSvc.DeepCopy(),
			setup: func(cs *cloudstackFake.CloudstackServer) {
				cs.AddLBRule("svc1.test.com", cloudstackFake.LoadBalancerRule{
					Rule: map[string]interface{}{
						"id":         "1",
						"name":       "svc1.test.com",
						"publicipid": "1",
						"publicip":   "192.168.1.100",
					},
				})
			},
			assert: func(t *testing.T, err error, cs *cloudstackFake.CloudstackServer) {
				require.NoError(t, err)
				cs.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
				})
			},
		},
		{
			name: "lb removal enabled on environment configs; lb rule without mandatory resource tags; should not manage",
			svc: func() *corev1.Service {
				svc := baseSvc.DeepCopy()
				svc.Annotations["environment-label"] = "env2"
				return svc
			}(),
			setup: func(cs *cloudstackFake.CloudstackServer) {
				cs.AddLBRule("svc1.env2.test.com", cloudstackFake.LoadBalancerRule{
					Rule: map[string]interface{}{
						"id":   "1",
						"name": "svc1.env2.test.com",
					},
				})
			},
			assert: func(t *testing.T, err error, cs *cloudstackFake.CloudstackServer) {
				require.NoError(t, err)
				cs.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.env2.test.com"}, "projectid": []string{"project-1"}, "listall": []string{"true"}}},
				})
			},
		},
		{
			name: "lb removal is enabled on service labels; lb rules without mandatory resource tags; should not manage",
			svc: func() *corev1.Service {
				svc := baseSvc.DeepCopy()
				svc.Annotations["csccm.cloudprovider.io/remove-loadbalancers-on-delete"] = "true"
				return svc
			}(),
			setup: func(cs *cloudstackFake.CloudstackServer) {
				cs.AddLBRule("svc1.test.com", cloudstackFake.LoadBalancerRule{
					Rule: map[string]interface{}{
						"id":         "1",
						"name":       "svc1.test.com",
						"publicipid": "1",
						"publicip":   "192.168.1.100",
					},
				})
			},
			assert: func(t *testing.T, err error, cs *cloudstackFake.CloudstackServer) {
				require.NoError(t, err)
				cs.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
				})
			},
		},
		{
			name: "lb removal is enabled on environment configs but disabled on service labels",
			svc: func() *corev1.Service {
				svc := baseSvc.DeepCopy()
				svc.Annotations["environment-label"] = "env2"
				svc.Annotations["csccm.cloudprovider.io/remove-loadbalancers-on-delete"] = "false"
				return svc
			}(),
			setup: func(cs *cloudstackFake.CloudstackServer) {
				cs.AddLBRule("svc1.env2.test.com", cloudstackFake.LoadBalancerRule{
					Rule: map[string]interface{}{
						"id":         "1",
						"name":       "svc1.env2.test.com",
						"publicipid": "1",
						"publicip":   "192.168.1.100",
						"tags": map[string]string{
							"cloudprovider":         "custom-cloudstack",
							"kubernetes_namespaces": "default",
							"kubernetes_service":    "svc1",
						},
					},
				})
			},
			assert: func(t *testing.T, err error, cs *cloudstackFake.CloudstackServer) {
				require.NoError(t, err)
				cs.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.env2.test.com"}}},
				})
			},
		},
		{
			name: "lb removal enabled on service labels and lb managed by this controller (custom-cloudstack)",
			svc: func() *corev1.Service {
				svc := baseSvc.DeepCopy()
				svc.Annotations["csccm.cloudprovider.io/remove-loadbalancers-on-delete"] = "true"
				return svc
			}(),
			setup: func(cs *cloudstackFake.CloudstackServer) {
				cs.AddTags("1", []cloudstack.Tags{
					{Key: "cloudprovider", Value: "custom-cloudstack"},
					{Key: "kubernetes_namespace", Value: "default"},
					{Key: "kubernetes_service", Value: "svc1"},
				})
				cs.AddIP(cloudstack.PublicIpAddress{
					Id:        "1",
					Ipaddress: "192.168.1.100",
					Tags: []cloudstack.Tags{
						{Key: "cloudprovider", Value: "custom-cloudstack"},
						{Key: "kubernetes_namespace", Value: "default"},
						{Key: "kubernetes_service", Value: "svc1"},
					},
				})
				cs.AddLBRule("svc1.test.com", cloudstackFake.LoadBalancerRule{
					Rule: map[string]interface{}{
						"id":         "1",
						"name":       "svc1.test.com",
						"publicipid": "1",
						"publicip":   "192.168.1.100",
						"tags": map[string]string{
							"cloudprovider":         "custom-cloudstack",
							"kubernetes_namespaces": "default",
							"kubernetes_service":    "svc1",
						},
					},
				})
			},
			assert: func(t *testing.T, err error, cs *cloudstackFake.CloudstackServer) {
				require.NoError(t, err)
				cs.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
					{Command: "deleteLoadBalancerRule", Params: url.Values{"id": []string{"1"}}},
					{Command: "queryAsyncJobResult", Params: url.Values{"jobid": []string{"job-delete-lb-1"}}},
					{Command: "listPublicIpAddresses", Params: url.Values{"id": []string{"1"}, "projectid": []string{"project-1"}, "listall": []string{"true"}}},
					{Command: "disassociateIpAddress", Params: url.Values{"id": []string{"1"}, "projectid": []string{"project-1"}}},
					{Command: "queryAsyncJobResult", Params: url.Values{"jobid": []string{"job-ip-disassociate-1"}}},
				})
			},
		},
		{
			name: "lb removal is enabled on environment config and it's managed by this controller",
			svc: func() *corev1.Service {
				svc := baseSvc.DeepCopy()
				svc.Annotations["environment-label"] = "env2"
				return svc
			}(),
			setup: func(cs *cloudstackFake.CloudstackServer) {
				cs.AddTags("1", []cloudstack.Tags{
					{Key: "cloudprovider", Value: "custom-cloudstack"},
					{Key: "kubernetes_namespace", Value: "default"},
					{Key: "kubernetes_service", Value: "svc1"},
				})
				cs.AddIP(cloudstack.PublicIpAddress{
					Id:        "1",
					Ipaddress: "192.168.1.100",
					Tags: []cloudstack.Tags{
						{Key: "cloudprovider", Value: "custom-cloudstack"},
						{Key: "kubernetes_namespace", Value: "default"},
						{Key: "kubernetes_service", Value: "svc1"},
					},
				})
				cs.AddLBRule("svc1.test.com", cloudstackFake.LoadBalancerRule{
					Rule: map[string]interface{}{
						"id":         "1",
						"name":       "svc1.env2.test.com",
						"publicipid": "1",
						"publicip":   "192.168.1.100",
						"tags": map[string]string{
							"cloudprovider":         "custom-cloudstack",
							"kubernetes_namespaces": "default",
							"kubernetes_service":    "svc1",
						},
					},
				})
			},
			assert: func(t *testing.T, err error, cs *cloudstackFake.CloudstackServer) {
				require.NoError(t, err)
				cs.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.env2.test.com"}}},
					{Command: "deleteLoadBalancerRule", Params: url.Values{"id": []string{"1"}}},
					{Command: "queryAsyncJobResult", Params: url.Values{"jobid": []string{"job-delete-lb-1"}}},
					{Command: "listPublicIpAddresses", Params: url.Values{"id": []string{"1"}, "projectid": []string{"project-1"}, "listall": []string{"true"}}},
					{Command: "disassociateIpAddress", Params: url.Values{"id": []string{"1"}, "projectid": []string{"project-1"}}},
					{Command: "queryAsyncJobResult", Params: url.Values{"jobid": []string{"job-ip-disassociate-1"}}},
				})
			},
		},
		{
			name: "lb removal is enabled; lb managed by this controller; no public ip attached to lb rule",
			svc: func() *corev1.Service {
				svc := baseSvc.DeepCopy()
				svc.Annotations["environment-label"] = "env2"
				return svc
			}(),
			setup: func(cs *cloudstackFake.CloudstackServer) {
				cs.AddTags("1", []cloudstack.Tags{
					{Key: "cloudprovider", Value: "custom-cloudstack"},
					{Key: "kubernetes_namespace", Value: "default"},
					{Key: "kubernetes_service", Value: "svc1"},
				})
				cs.AddLBRule("svc1.test.com", cloudstackFake.LoadBalancerRule{
					Rule: map[string]interface{}{
						"id":   "1",
						"name": "svc1.env2.test.com",
						"tags": map[string]string{
							"cloudprovider":         "custom-cloudstack",
							"kubernetes_namespaces": "default",
							"kubernetes_service":    "svc1",
						},
					},
				})
			},
			assert: func(t *testing.T, err error, cs *cloudstackFake.CloudstackServer) {
				require.NoError(t, err)
				cs.HasCalls(t, []cloudstackFake.MockAPICall{
					{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.env2.test.com"}}},
					{Command: "deleteLoadBalancerRule", Params: url.Values{"id": []string{"1"}}},
					{Command: "queryAsyncJobResult", Params: url.Values{"jobid": []string{"job-delete-lb-1"}}},
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotNil(t, tt.assert)
			srv := cloudstackFake.NewCloudstackServer()
			defer srv.Close()
			if tt.setup != nil {
				tt.setup(srv)
			}
			csCloud := newTestCSCloud(t, &CSConfig{
				Global: globalConfig{
					EnvironmentLabel: "environment-label",
					ProjectIDLabel:   "project-label",
				},
				Environment: map[string]*environmentConfig{
					"env1": {
						APIURL:          srv.URL,
						APIKey:          "a",
						SecretKey:       "b",
						LBEnvironmentID: "1",
						LBDomain:        "test.com",
					},
					"env2": {
						APIURL:          srv.URL,
						APIKey:          "a",
						SecretKey:       "b",
						LBEnvironmentID: "2",
						LBDomain:        "env2.test.com",
						RemoveLBs:       true,
					},
				},
			}, nil)
			err := csCloud.EnsureLoadBalancerDeleted(context.Background(), "cluster1", tt.svc)
			tt.assert(t, err, srv)
		})
	}
}

func TestCheckLoadBalancerRule(t *testing.T) {
	tests := []struct {
		svcPorts     []corev1.ServicePort
		rule         *loadBalancerRule
		existing     bool
		needsUpdate  bool
		needsTags    bool
		deleteCalled bool
		deleteError  bool
		errorMatches string
		annotations  map[string]string
		prepend      *kubeFake.Clientset
	}{
		{
			existing:    false,
			needsUpdate: false,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 1234, NodePort: 4567, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1234",
					Privateport: "4567",
					Publicip:    "10.0.0.1",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:    true,
			needsUpdate: false,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 1234, NodePort: 4567, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1234",
					Privateport: "4567",
					Publicip:    "10.0.0.1",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:    true,
			needsUpdate: false,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 5, NodePort: 6, Protocol: corev1.ProtocolTCP},
				{Port: 2, NodePort: 20, Protocol: corev1.ProtocolTCP},
				{Port: 10, NodePort: 5, Protocol: corev1.ProtocolTCP},
				{Port: 3, NodePort: 4, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{
					"3:4",
					"5:6",
					"10:5",
				},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "2",
					Privateport: "20",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:    true,
			needsUpdate: false,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 5, NodePort: 6, Protocol: corev1.ProtocolTCP},
				{Port: 2, NodePort: 20, Protocol: corev1.ProtocolTCP},
				{Port: 10, NodePort: 5, Protocol: corev1.ProtocolTCP},
				{Port: 3, NodePort: 4, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Id:          "id-1",
					Publicport:  "2",
					Privateport: "20",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
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
				{Port: 1, NodePort: 2, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1",
					Privateport: "2",
					Algorithm:   "x",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
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
				{Port: 1, NodePort: 2, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1",
					Privateport: "2",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag},
					},
				},
			},
			existing:     true,
			needsUpdate:  false,
			needsTags:    true,
			deleteCalled: false,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 1, NodePort: 2, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "other",
					Publicport:  "1",
					Privateport: "2",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag},
					},
				},
			},
			existing:     false,
			needsUpdate:  false,
			deleteCalled: true,
		},
		{
			annotations: map[string]string{"csccm.cloudprovider.io/loadbalancer-use-targetport": "true"},
			svcPorts: []corev1.ServicePort{
				{Port: 1, NodePort: 2, TargetPort: intstr.IntOrString{IntVal: 10}, Protocol: corev1.ProtocolTCP},
				{Port: 2, NodePort: 20, TargetPort: intstr.IntOrString{IntVal: 40}, Protocol: corev1.ProtocolTCP},
				{Port: 3, NodePort: 30, TargetPort: intstr.IntOrString{IntVal: 50}, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{
					"2:40",
					"3:50",
				},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1",
					Privateport: "10",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:     true,
			needsUpdate:  false,
			deleteCalled: false,
		},
		{
			prepend: kubeFake.NewSimpleClientset(&corev1.EndpointsList{
				Items: []corev1.Endpoints{
					{ObjectMeta: metav1.ObjectMeta{
						Name:      serviceTag,
						Namespace: namespaceTag,
					},
						Subsets: []corev1.EndpointSubset{{Ports: []corev1.EndpointPort{{Name: "http", Port: 8080}, {Name: "https", Port: 8443}}}}},
				}}),
			annotations: map[string]string{"csccm.cloudprovider.io/loadbalancer-use-targetport": "true"},
			svcPorts: []corev1.ServicePort{
				{Port: 80, NodePort: 2, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP},
				{Port: 443, NodePort: 20, TargetPort: intstr.FromString("https"), Protocol: corev1.ProtocolTCP},
				{Port: 3, NodePort: 30, TargetPort: intstr.FromString("40"), Protocol: corev1.ProtocolTCP},
				{Port: 4, NodePort: 30, TargetPort: intstr.FromInt(50), Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{
					"4:50",
					"80:8080",
					"443:8443",
				},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "3",
					Privateport: "40",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:     true,
			needsUpdate:  false,
			deleteCalled: false,
		},
		{
			prepend: kubeFake.NewSimpleClientset(&corev1.EndpointsList{
				Items: []corev1.Endpoints{
					{ObjectMeta: metav1.ObjectMeta{
						Name:      serviceTag,
						Namespace: namespaceTag,
					},
						Subsets: []corev1.EndpointSubset{{Ports: []corev1.EndpointPort{{Name: "https", Port: 8443}}}}},
				}}),
			annotations: map[string]string{"csccm.cloudprovider.io/loadbalancer-use-targetport": "true"},
			svcPorts: []corev1.ServicePort{
				{Port: 80, NodePort: 2, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP},
				{Port: 443, NodePort: 20, TargetPort: intstr.FromString("https"), Protocol: corev1.ProtocolTCP},
				{Port: 3, NodePort: 30, TargetPort: intstr.FromString("40"), Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{
					"80:8080",
					"443:8443",
				},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "3",
					Privateport: "40",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:     false,
			needsUpdate:  false,
			deleteCalled: false,
			errorMatches: `no port name "http" found for endpoint for lb\(test, nil, lbrule\(, test\), ip\(, \), svc\(kubernetes_namespace/kubernetes_service\)\)`,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 1, NodePort: 2, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1",
					Privateport: "2",
					Protocol:    "UDP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag},
					},
				},
			},
			existing:     false,
			needsUpdate:  false,
			deleteCalled: true,
		},
		{
			svcPorts: []corev1.ServicePort{
				{Port: 4, NodePort: 40, Protocol: corev1.ProtocolTCP},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Id:          "id-1",
					Publicport:  "2",
					Privateport: "20",
					Protocol:    "TCP",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:     false,
			needsUpdate:  false,
			deleteError:  true,
			deleteCalled: true,
			errorMatches: `error deleting load balancer rule lb\(test, nil, lbrule\(id-1, test\), ip\(, \), svc\(kubernetes_namespace/kubernetes_service\)\): CloudStack API error 0 \(CSExceptionErrorCode: 0\): my delete error`,
		},
	}

	var deleteCalled bool
	var deleteError bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Query().Get("command"), "deleteLoadBalancerRule")
		deleteCalled = true
		if deleteError {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"deleteLoadBalancerRule":{"errortext": "my delete error"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"deleteLoadBalancerRule":{}}`))
	}))
	defer srv.Close()
	fakeCli := cloudstack.NewAsyncClient(srv.URL, "", "", false)
	cloud := &CSCloud{
		environments: map[string]CSEnvironment{
			"test": {
				client: fakeCli,
			},
		},
	}
	for _, tt := range tests {
		deleteCalled = false
		deleteError = false
		if tt.prepend != nil {
			cloud.kubeClient = tt.prepend
		} else {
			cloud.kubeClient = kubeFake.NewSimpleClientset()
		}
		lb := loadBalancer{
			name: "test",
			cloud: &projectCloud{
				environment: "test",
				CSCloud:     cloud,
			},
			rule: tt.rule,
		}
		t.Run("", func(t *testing.T) {
			deleteError = tt.deleteError
			lb.service = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceTag, Namespace: namespaceTag, Annotations: tt.annotations},
				Spec:       corev1.ServiceSpec{Ports: tt.svcPorts},
			}
			result, err := lb.checkLoadBalancerRule()
			if tt.errorMatches != "" {
				assert.Error(t, err)
				assert.Regexp(t, tt.errorMatches, err.Error())
				assert.False(t, result.exists)
				assert.False(t, result.needsUpdate)
				assert.False(t, result.needsTags)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.existing, result.exists)
				assert.Equal(t, tt.needsUpdate, result.needsUpdate)
				assert.Equal(t, tt.needsTags, result.needsTags)
			}
			assert.Equal(t, tt.deleteCalled, deleteCalled)
		})
	}
}

func Test_listAllIPPages(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		srv := cloudstackFake.NewCloudstackServer()
		defer srv.Close()
		csCli := cloudstack.NewAsyncClient(srv.URL, "a", "b", false)
		ips, err := listAllIPPages(csCli, &cloudstack.ListPublicIpAddressesParams{})
		require.NoError(t, err)
		assert.Equal(t, []*cloudstack.PublicIpAddress{}, ips)
	})

	t.Run("remains safe even with cloudstack weirdness", func(t *testing.T) {
		srv := cloudstackFake.NewCloudstackServer()
		defer srv.Close()
		var pages []string
		srv.Hook = func(w http.ResponseWriter, r *http.Request) bool {
			cmd := r.FormValue("command")
			if cmd == "listPublicIpAddresses" {
				page := r.FormValue("page")
				pages = append(pages, page)
				w.Write([]byte(`{
"listPublicIpAddressesResponse": {
	"count": 4,
	"publicipaddress": [
		{"id": "id1", "ipaddress": "ip1"},
		{"id": "id2", "ipaddress": "ip2"},
		{"id": "id3", "ipaddress": "ip3"}
	]
}}`))
				return true
			}
			return false
		}
		csCli := cloudstack.NewAsyncClient(srv.URL, "a", "b", false)
		ips, err := listAllIPPages(csCli, &cloudstack.ListPublicIpAddressesParams{})
		require.NoError(t, err)
		assert.Equal(t, []string{"1", "2"}, pages)
		assert.Equal(t, []*cloudstack.PublicIpAddress{
			{Id: "id1", Ipaddress: "ip1"},
			{Id: "id2", Ipaddress: "ip2"},
			{Id: "id3", Ipaddress: "ip3"},
		}, ips)
	})
}
