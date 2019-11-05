package cloudstack

import (
	"context"
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
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeFake "k8s.io/client-go/kubernetes/fake"
)

func init() {
	flag.Set("v", "10")
	flag.Set("logtostderr", "true")
}

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
			{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
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
		name    string
		prepend *kubeFake.Clientset
		hook    func(t *testing.T, srv *cloudstackFake.CloudstackServer)
		calls   []consecutiveCall
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
			name: "add extra params for associateIPAddress",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Labels["csccm.cloudprovider.io/associateipaddress-extra-param-foo"] = "bar"
						svc.Annotations["csccm.cloudprovider.io/associateipaddress-extra-param-lbenvironmentid"] = "3"
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
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
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
			name: "add extra params for createLoadBalancerRule",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/createloadbalancer-extra-param-dsr"] = "true"
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
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.Error(t, err)
						assert.Contains(t, err.Error(), "error list load balancer pools for svc1.test.com: no LB pools found")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
							{Command: "assignNetworkToLBRule", Params: url.Values{"id": []string{"lbrule-1"}, "networkids": []string{"net1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "assignToLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}, "virtualmachineids": []string{"vm1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
						})
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
							{Command: "listGloboNetworkPools", Params: url.Values{"lbruleid": []string{"lbrule-1"}}},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"0"}, "healthchecktype": []string{"HTTP"}, "healthcheck": []string{"GET / HTTP/1.0"}, "expectedhealthcheck": []string{"200 OK"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "updateGloboNetworkPool", Params: url.Values{"lbruleid": []string{"lbrule-1"}, "poolids": []string{"1"}, "healthchecktype": []string{"HTTPS"}, "healthcheck": []string{"GET /test HTTP/1.0"}, "expectedhealthcheck": []string{"bleh"}}},
							{Command: "queryAsyncJobResult"},
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
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
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
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
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
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
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
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
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
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "publicport": []string{"8080"}, "additionalportmap": []string{"8443:30002"}, "privateport": []string{"30001"}}},
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
						svc.Spec.Ports = []corev1.ServicePort{{Port: 8080, NodePort: 30001, Protocol: corev1.ProtocolTCP},
							{Port: 8443, NodePort: 30002, Protocol: corev1.ProtocolTCP}}
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
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "foo.bar.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.1", Hostname: "svc1.anotherrealm.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
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
			name: "set use-targetport annotation creates LB using targetport same as destination",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Annotations["csccm.cloudprovider.io/loadbalancer-use-targetport"] = "true"
						svc.Spec.Ports = []corev1.ServicePort{{Port: 80, NodePort: 30002, TargetPort: intstr.IntOrString{IntVal: 80}, Protocol: corev1.ProtocolTCP}, {Port: 443, NodePort: 30003, TargetPort: intstr.IntOrString{IntVal: 443}, Protocol: corev1.ProtocolTCP}}
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
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
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
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"ip-1"}, "privateport": []string{"30003"}}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"page": []string{"0"}}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "10.0.0.2", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
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
			name: "service requesting explicit IP, ip not found",
			calls: []consecutiveCall{
				{
					svc: (func() corev1.Service {
						svc := baseSvc.DeepCopy()
						svc.Spec.LoadBalancerIP = "192.168.9.9"
						return *svc
					})(),
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.Error(t, err)
						assert.Contains(t, err.Error(), "could not find IP address 192.168.9.9")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listLoadBalancerRules", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"page": []string{"0"}, "tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
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
							{Command: "listPublicIpAddresses", Params: url.Values{"page": []string{"0"}, "tags[0].key": nil, "tags[1].key": nil, "tags[2].key": nil}},
							{Command: "listPublicIpAddresses", Params: url.Values{"ipaddress": []string{"192.168.9.9"}}},
							{Command: "createLoadBalancerRule", Params: url.Values{"name": []string{"svc1.test.com"}, "publicipid": []string{"mycustomip"}, "privateport": []string{"30001"}}},
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
							{Command: "updateLoadBalancerRule", Params: url.Values{"id": []string{"lbrule-1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"cloudprovider"}, "tags[0].value": []string{"custom-cloudstack"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_namespace"}, "tags[0].value": []string{"myns"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "createTags", Params: url.Values{"tags[0].key": []string{"kubernetes_service"}, "tags[0].value": []string{"svc1"}}},
							{Command: "queryAsyncJobResult"},
							{Command: "listLoadBalancerRuleInstances", Params: url.Values{"page": []string{"0"}, "id": []string{"lbrule-1"}}},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.NoError(t, err)
						assert.Equal(t, lbStatus, &corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{
								{IP: "192.168.9.9", Hostname: "svc1.test.com"},
							},
						})
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
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
					assert: func(t *testing.T, srv *cloudstackFake.CloudstackServer, lbStatus *v1.LoadBalancerStatus, err error) {
						require.Error(t, err)
						assert.Contains(t, err.Error(), "could not find IP address 192.168.9.9")
						srv.HasCalls(t, []cloudstackFake.MockAPICall{
							{Command: "listVirtualMachines"},
							{Command: "listLoadBalancerRules", Params: url.Values{"keyword": []string{"svc1.test.com"}}},
							{Command: "listPublicIpAddresses", Params: url.Values{"ipaddress": []string{"192.168.9.9"}}},
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
			csCloud, err := newCSCloud(&CSConfig{
				Global: globalConfig{
					EnvironmentLabel: "environment-label",
					ProjectIDLabel:   "project-label",
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
			})
			require.Nil(t, err)
			if tt.hook != nil {
				tt.hook(t, srv)
			}
			if tt.prepend != nil {
				csCloud.kubeClient = tt.prepend
			}
			var lbStatus *corev1.LoadBalancerStatus
			for i, cc := range tt.calls {
				t.Logf("call %d", i)
				svc := cc.svc.DeepCopy()
				if err == nil && lbStatus != nil {
					svc.Status.LoadBalancer = *lbStatus
				}
				lbStatus, err = csCloud.EnsureLoadBalancer(context.Background(), "kubernetes", svc, baseNodes)
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
		{"matchSingle", CSCloud{
			config: CSConfig{
				Global: globalConfig{
					ServiceFilterLabel: "app-pool",
					NodeFilterLabel:    "pool",
				},
			},
		}, s1, nodes[0:1]},
		{"emptyLabels", CSCloud{}, s1, nodes},
		{"matchMultiple", CSCloud{
			config: CSConfig{
				Global: globalConfig{
					ServiceFilterLabel: "app-pool",
					NodeFilterLabel:    "pool",
				},
			},
		}, s2, nodes[1:3]},
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
		svcPorts     []corev1.ServicePort
		rule         *loadBalancerRule
		existing     bool
		needsUpdate  bool
		deleteCalled bool
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
				{Port: 1234, NodePort: 4567},
			},
			rule: &loadBalancerRule{
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1234",
					Privateport: "4567",
					Publicip:    "10.0.0.1",
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
				{Port: 1234, NodePort: 4567},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1234",
					Privateport: "4567",
					Publicip:    "10.0.0.1",
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
					Name:        "test",
					Publicport:  "2",
					Privateport: "20",
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
				{Port: 5, NodePort: 6},
				{Port: 2, NodePort: 20},
				{Port: 10, NodePort: 5},
				{Port: 3, NodePort: 4},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Id:          "id-1",
					Publicport:  "2",
					Privateport: "20",
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
				{Port: 1, NodePort: 2},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1",
					Privateport: "2",
					Algorithm:   "x",
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
				{Port: 1, NodePort: 2},
			},
			rule: &loadBalancerRule{
				AdditionalPortMap: []string{},
				LoadBalancerRule: &cloudstack.LoadBalancerRule{
					Name:        "test",
					Publicport:  "1",
					Privateport: "2",
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag},
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
					Name:        "other",
					Publicport:  "1",
					Privateport: "2",
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
				{Port: 1, NodePort: 2, TargetPort: intstr.IntOrString{IntVal: 10}},
				{Port: 2, NodePort: 20, TargetPort: intstr.IntOrString{IntVal: 40}},
				{Port: 3, NodePort: 30, TargetPort: intstr.IntOrString{IntVal: 50}},
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
				{Port: 80, NodePort: 2, TargetPort: intstr.FromString("http")},
				{Port: 443, NodePort: 20, TargetPort: intstr.FromString("https")},
				{Port: 3, NodePort: 30, TargetPort: intstr.FromString("40")},
				{Port: 4, NodePort: 30, TargetPort: intstr.FromInt(50)},
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
				{Port: 80, NodePort: 2, TargetPort: intstr.FromString("http")},
				{Port: 443, NodePort: 20, TargetPort: intstr.FromString("https")},
				{Port: 3, NodePort: 30, TargetPort: intstr.FromString("40")},
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
					Tags: []cloudstack.Tags{
						{Key: serviceTag}, {Key: cloudProviderTag}, {Key: namespaceTag},
					},
				},
			},
			existing:     false,
			needsUpdate:  false,
			deleteCalled: false,
			errorMatches: "Error resolving target port: no port name \"http\" found for endpoint service kubernetes_service on namespace kubernetes_namespace",
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
			"test": {
				client: fakeCli,
			},
		},
	}
	for i, tt := range tests {
		deleteCalled = false
		if tt.prepend != nil {
			cloud.kubeClient = tt.prepend
		}
		lb := loadBalancer{
			name: "test",
			cloud: &projectCloud{
				environment: "test",
				CSCloud:     cloud,
			},
			rule: tt.rule,
		}
		t.Run(fmt.Sprintf("test %d", i), func(t *testing.T) {
			existing, needsUpdate, err := lb.checkLoadBalancerRule("name", &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: serviceTag, Namespace: namespaceTag, Annotations: tt.annotations},
				Spec: corev1.ServiceSpec{Ports: tt.svcPorts}})
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
		assert.Equal(t, []string{"0", "1"}, pages)
		assert.Equal(t, []*cloudstack.PublicIpAddress{
			{Id: "id1", Ipaddress: "ip1"},
			{Id: "id2", Ipaddress: "ip2"},
			{Id: "id3", Ipaddress: "ip3"},
		}, ips)
	})
}
