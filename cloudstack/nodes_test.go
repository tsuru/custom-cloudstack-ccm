package cloudstack

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cloudstackFake "github.com/tsuru/custom-cloudstack-ccm/cloudstack/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_NodeRegistry_updateNodes(t *testing.T) {
	tests := []struct {
		name        string
		nodes       []*corev1.Node
		nodesAgain  []*corev1.Node
		expected    map[string]*nodeInfo
		expectedErr string
		hook        func(*CSConfig) *CSConfig
	}{
		{
			name:        "with nil nodes",
			expectedErr: `no nodes available to add to load balancer`,
			expected:    map[string]*nodeInfo{},
		},
		{
			name:        "with empty nodes",
			nodes:       []*corev1.Node{},
			expectedErr: `no nodes available to add to load balancer`,
			expected:    map[string]*nodeInfo{},
		},
		{
			name: "with no nodes found in cloudstack",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "notfound",
						Labels: map[string]string{
							"my/project-label":  "11111111-2222-3333-4444-555555555555",
							"environment-label": "env1",
							"pool-label":        "pool1",
						},
					},
				},
			},
			expectedErr: `unable to map kubernetes nodes to cloudstack instances for nodes: notfound`,
			expected:    map[string]*nodeInfo{},
		},
		{
			name: "with some nodes not found in cloudstack",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "notfound",
						Labels: map[string]string{
							"my/project-label":  "11111111-2222-3333-4444-555555555555",
							"environment-label": "env1",
							"pool-label":        "pool1",
						},
					},
				},
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
			},
			expected: map[string]*nodeInfo{
				"n1": {
					name:          "n1",
					vmID:          "vm1",
					networkID:     "net1",
					hostName:      "n1",
					projectID:     "11111111-2222-3333-4444-555555555555",
					environmentID: "env1",
					filterValue:   "pool1",
					revision:      1,
				},
			},
		},
		{
			name: "with nodes found in cloudstack",
			nodes: []*corev1.Node{
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
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n2",
						Labels: map[string]string{
							"my/project-label":  "91111111-2222-3333-4444-555555555555",
							"environment-label": "env1",
							"pool-label":        "pool2",
						},
					},
				},
			},
			expected: map[string]*nodeInfo{
				"n1": {
					name:          "n1",
					vmID:          "vm1",
					networkID:     "net1",
					hostName:      "n1",
					projectID:     "11111111-2222-3333-4444-555555555555",
					environmentID: "env1",
					filterValue:   "pool1",
					revision:      1,
				},
				"n2": {
					name:          "n2",
					vmID:          "vm2",
					networkID:     "net2",
					hostName:      "n2",
					projectID:     "91111111-2222-3333-4444-555555555555",
					environmentID: "env1",
					filterValue:   "pool2",
					revision:      1,
				},
			},
		},
		{
			name: "node with default project",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n1",
						Labels: map[string]string{
							"environment-label": "env1",
							"pool-label":        "pool1",
						},
					},
				},
			},
			expected: map[string]*nodeInfo{
				"n1": {
					name:          "n1",
					vmID:          "vm1",
					networkID:     "net1",
					hostName:      "n1",
					projectID:     "def-proj1",
					environmentID: "env1",
					filterValue:   "pool1",
					revision:      1,
				},
			},
		},
		{
			name: "node with default environment",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n1",
						Labels: map[string]string{
							"pool-label": "pool1",
						},
					},
				},
			},
			expected: map[string]*nodeInfo{
				"n1": {
					name:          "n1",
					vmID:          "vm1",
					networkID:     "net1",
					hostName:      "n1",
					projectID:     "def-proj1",
					environmentID: "env1",
					filterValue:   "pool1",
					revision:      1,
				},
			},
		},
		{
			name: "node with empty filter value",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n1",
					},
				},
			},
			expected: map[string]*nodeInfo{
				"n1": {
					name:          "n1",
					vmID:          "vm1",
					networkID:     "net1",
					hostName:      "n1",
					projectID:     "def-proj1",
					environmentID: "env1",
					filterValue:   "",
					revision:      1,
				},
			},
		},
		{
			name: "node with multiple environments",
			hook: func(cfg *CSConfig) *CSConfig {
				cfg.Environment["env2"] = cfg.Environment["env1"]
				cfg.Environment["env2"].ProjectID = "def-proj2"
				return cfg
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n1",
					},
				},
			},
			expectedErr: `unable to retrieve projectID for node "n1" in environment "": no projectID found`,
			expected:    map[string]*nodeInfo{},
		},
		{
			name: "with existing nodes updating labels",
			nodes: []*corev1.Node{
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
			},
			nodesAgain: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n1",
						Labels: map[string]string{
							"my/project-label":  "91111111-2222-3333-4444-555555555555",
							"environment-label": "env1",
							"pool-label":        "pool2",
						},
					},
				},
			},
			expected: map[string]*nodeInfo{
				"n1": {
					name:          "n1",
					vmID:          "vm1",
					networkID:     "net1",
					hostName:      "n1",
					projectID:     "91111111-2222-3333-4444-555555555555",
					environmentID: "env1",
					filterValue:   "pool2",
					revision:      1,
				},
			},
		},
		{
			name: "with new nodes in second call",
			nodes: []*corev1.Node{
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
			},
			nodesAgain: []*corev1.Node{
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
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n2",
						Labels: map[string]string{
							"my/project-label":  "91111111-2222-3333-4444-555555555555",
							"environment-label": "env1",
							"pool-label":        "pool2",
						},
					},
				},
			},
			expected: map[string]*nodeInfo{
				"n1": {
					name:          "n1",
					vmID:          "vm1",
					networkID:     "net1",
					hostName:      "n1",
					projectID:     "11111111-2222-3333-4444-555555555555",
					environmentID: "env1",
					filterValue:   "pool1",
					revision:      1,
				},
				"n2": {
					name:          "n2",
					vmID:          "vm2",
					networkID:     "net2",
					hostName:      "n2",
					projectID:     "91111111-2222-3333-4444-555555555555",
					environmentID: "env1",
					filterValue:   "pool2",
					revision:      2,
				},
			},
		},
		{
			name: "with fewer nodes in second call",
			nodes: []*corev1.Node{
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
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n2",
						Labels: map[string]string{
							"my/project-label":  "91111111-2222-3333-4444-555555555555",
							"environment-label": "env1",
							"pool-label":        "pool2",
						},
					},
				},
			},
			nodesAgain: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n2",
						Labels: map[string]string{
							"my/project-label":  "91111111-2222-3333-4444-555555555555",
							"environment-label": "env1",
							"pool-label":        "pool2",
						},
					},
				},
			},
			expected: map[string]*nodeInfo{
				"n2": {
					name:          "n2",
					vmID:          "vm2",
					networkID:     "net2",
					hostName:      "n2",
					projectID:     "91111111-2222-3333-4444-555555555555",
					environmentID: "env1",
					filterValue:   "pool2",
					revision:      1,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := cloudstackFake.NewCloudstackServer()
			defer srv.Close()
			if tt.hook == nil {
				tt.hook = func(cfg *CSConfig) *CSConfig { return cfg }
			}
			cs := newTestCSCloud(t, tt.hook(&CSConfig{
				Global: globalConfig{
					EnvironmentLabel:   "environment-label",
					ProjectIDLabel:     "my/project-label",
					NodeFilterLabel:    "pool-label",
					ServiceFilterLabel: "pool-label",
				},
				Environment: map[string]*environmentConfig{
					"env1": {
						APIURL:          srv.URL,
						APIKey:          "a",
						SecretKey:       "b",
						LBEnvironmentID: "1",
						LBDomain:        "test.com",
						ProjectID:       "def-proj1",
					},
				},
			}), nil)

			err := cs.nodeRegistry.updateNodes(tt.nodes)
			if tt.expectedErr == "" {
				require.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.expectedErr)
			}
			if tt.nodesAgain != nil {
				err = cs.nodeRegistry.updateNodes(tt.nodesAgain)
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expected, cs.nodeRegistry.nodes)
		})
	}
}

func Test_NodeRegistry_nodesContainingService(t *testing.T) {
	tests := []struct {
		name          string
		endpoints     []corev1.Endpoints
		nodes         []*corev1.Node
		svc           serviceKey
		expectedNodes []string
	}{
		{
			name: "empty",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "n1"},
				},
			},
		},
		{
			name: "with svc key",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "n1"},
				},
			},
			svc: serviceKey{"ns1", "s1"},
		},
		{
			name: "single svc pointing to node",
			endpoints: []corev1.Endpoints{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s1"},
					Subsets: []corev1.EndpointSubset{
						{
							Addresses: []corev1.EndpointAddress{
								{
									NodeName: strPtr("n1"),
								},
							},
						},
					},
				},
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n1",
					},
				},
			},
			svc:           serviceKey{"ns1", "s1"},
			expectedNodes: []string{"n1"},
		},
		{
			name: "target svc in another namespace",
			endpoints: []corev1.Endpoints{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s1"},
					Subsets: []corev1.EndpointSubset{
						{
							Addresses: []corev1.EndpointAddress{
								{
									NodeName: strPtr("n1"),
								},
							},
						},
					},
				},
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n1",
					},
				},
			},
			svc:           serviceKey{"ns2", "s1"},
			expectedNodes: nil,
		},
		{
			name: "endpoints poiting to multiple nodes",
			endpoints: []corev1.Endpoints{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s1"},
					Subsets: []corev1.EndpointSubset{
						{
							Addresses: []corev1.EndpointAddress{
								{
									NodeName: strPtr("n1"),
								},
								{
									NodeName: strPtr("n2"),
								},
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s2"},
					Subsets: []corev1.EndpointSubset{
						{
							Addresses: []corev1.EndpointAddress{
								{
									NodeName: strPtr("n3"),
								},
							},
						},
					},
				},
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n1",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n2",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n3",
					},
				},
			},
			svc:           serviceKey{"ns1", "s1"},
			expectedNodes: []string{"n1", "n2"},
		},
		{
			name: "endpoints poiting to invalid node",
			endpoints: []corev1.Endpoints{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s1"},
					Subsets: []corev1.EndpointSubset{
						{
							Addresses: []corev1.EndpointAddress{
								{
									NodeName: strPtr("n1"),
								},
								{
									NodeName: strPtr("n2"),
								},
							},
						},
					},
				},
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "n1",
					},
				},
			},
			svc:           serviceKey{"ns1", "s1"},
			expectedNodes: []string{"n1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := cloudstackFake.NewCloudstackServer()
			defer srv.Close()
			cs := newTestCSCloud(t, &CSConfig{
				Global: globalConfig{
					EnvironmentLabel:   "environment-label",
					ProjectIDLabel:     "my/project-label",
					NodeFilterLabel:    "pool-label",
					ServiceFilterLabel: "pool-label",
				},
				Environment: map[string]*environmentConfig{
					"env1": {
						APIURL:          srv.URL,
						APIKey:          "a",
						SecretKey:       "b",
						LBEnvironmentID: "1",
						LBDomain:        "test.com",
						ProjectID:       "default-proj",
					},
				},
			}, nil)

			err := cs.nodeRegistry.updateNodes(tt.nodes)
			require.NoError(t, err)
			for _, ep := range tt.endpoints {
				cs.nodeRegistry.updateEndpointsNodes(&ep)
			}
			result := cs.nodeRegistry.nodesContainingService(tt.svc)
			var nodeNames []string
			for _, n := range result {
				nodeNames = append(nodeNames, n.name)
			}
			sort.Strings(nodeNames)
			sort.Strings(tt.expectedNodes)
			assert.Equal(t, tt.expectedNodes, nodeNames)
		})
	}
}

func Test_NodeRegistry_nodesForService(t *testing.T) {
	tests := []struct {
		name          string
		nodes         []*corev1.Node
		svc           *corev1.Service
		expectedNodes []string
		expectedErr   string
	}{
		{
			name: "empty",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "n1"},
				},
			},
			expectedErr: "service cannot be nil",
		},
		{
			name: "svc with no labels",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "n1"},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1"},
			},
			expectedNodes: []string{"n1"},
		},
		{
			name: "svc filtering by environment, no matches",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "n1"},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns1",
					Name:      "svc1",
					Labels: map[string]string{
						"environment-label": "env2",
					},
				},
			},
			expectedErr: "no nodes available to add to service ns1/svc1",
		},
		{
			name: "svc filtering by project",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "n1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "n2", Labels: map[string]string{
						"my/project-label": "p9",
					}},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns1",
					Name:      "svc1",
					Labels: map[string]string{
						"my/project-label": "p9",
					},
				},
			},
			expectedNodes: []string{"n2"},
		},
		{
			name: "svc filtering by filterValue",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "n1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "n2", Labels: map[string]string{
						"pool-label": "p1",
					}},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns1",
					Name:      "svc1",
					Labels: map[string]string{
						"pool-label": "p1",
					},
				},
			},
			expectedNodes: []string{"n2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := cloudstackFake.NewCloudstackServer()
			defer srv.Close()
			cs := newTestCSCloud(t, &CSConfig{
				Global: globalConfig{
					EnvironmentLabel:   "environment-label",
					ProjectIDLabel:     "my/project-label",
					NodeFilterLabel:    "pool-label",
					ServiceFilterLabel: "pool-label",
				},
				Environment: map[string]*environmentConfig{
					"env1": {
						APIURL:          srv.URL,
						APIKey:          "a",
						SecretKey:       "b",
						LBEnvironmentID: "1",
						LBDomain:        "test.com",
						ProjectID:       "default-proj",
					},
				},
			}, nil)

			err := cs.nodeRegistry.updateNodes(tt.nodes)
			require.NoError(t, err)
			result, err := cs.nodeRegistry.nodesForService(tt.svc)
			if tt.expectedErr == "" {
				require.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.expectedErr)
			}
			var nodeNames []string
			for _, n := range result {
				nodeNames = append(nodeNames, n.name)
			}
			sort.Strings(nodeNames)
			sort.Strings(tt.expectedNodes)
			assert.Equal(t, tt.expectedNodes, nodeNames)
		})
	}
}

func strPtr(v string) *string {
	return &v
}
