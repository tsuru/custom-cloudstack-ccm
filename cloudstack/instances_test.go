package cloudstack

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cloudstackFake "github.com/tsuru/custom-cloudstack-ccm/cloudstack/fake"
	"github.com/xanzy/go-cloudstack/cloudstack"
	v1 "k8s.io/api/core/v1"
)

func Test_CSCloud_nodeAddresses(t *testing.T) {
	tests := []struct {
		config   globalConfig
		vm       cloudstack.VirtualMachine
		expected []v1.NodeAddress
	}{
		{
			vm: cloudstack.VirtualMachine{
				Id:       "vm1",
				Hostname: "host1",
				Nic: []cloudstack.Nic{
					{Ipaddress: "10.0.0.1"},
				},
			},
			expected: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeHostName, Address: "host1"},
			},
		},
		{
			vm: cloudstack.VirtualMachine{
				Id:       "vm1",
				Hostname: "host1",
				Nic: []cloudstack.Nic{
					{Ipaddress: "10.0.0.1"},
				},
				Publicip: "200.0.0.1",
			},
			expected: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeHostName, Address: "host1"},
				{Type: v1.NodeExternalIP, Address: "200.0.0.1"},
			},
		},
		{
			config: globalConfig{
				InternalIPIndex: 1,
				ExternalIPIndex: 0,
			},
			vm: cloudstack.VirtualMachine{
				Id:       "vm1",
				Hostname: "host1",
				Nic: []cloudstack.Nic{
					{Ipaddress: "10.0.0.1"},
				},
			},
			expected: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeHostName, Address: "host1"},
			},
		},
		{
			config: globalConfig{
				InternalIPIndex: 1,
				ExternalIPIndex: 0,
			},
			vm: cloudstack.VirtualMachine{
				Id:       "vm1",
				Hostname: "host1",
				Nic: []cloudstack.Nic{
					{Ipaddress: "10.0.0.1"},
				},
				Publicip: "200.0.0.1",
			},
			expected: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeHostName, Address: "host1"},
				{Type: v1.NodeExternalIP, Address: "200.0.0.1"},
			},
		},
		{
			config: globalConfig{
				InternalIPIndex: 1,
				ExternalIPIndex: 0,
			},
			vm: cloudstack.VirtualMachine{
				Id:       "vm1",
				Hostname: "host1",
				Nic: []cloudstack.Nic{
					{Ipaddress: "10.0.0.1"},
					{Ipaddress: "192.0.0.1"},
				},
			},
			expected: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "192.0.0.1"},
				{Type: v1.NodeHostName, Address: "host1"},
				{Type: v1.NodeExternalIP, Address: "10.0.0.1"},
			},
		},
		{
			config: globalConfig{
				InternalIPIndex: 9,
			},
			vm: cloudstack.VirtualMachine{
				Id:       "vm1",
				Hostname: "host1",
				Nic: []cloudstack.Nic{
					{Ipaddress: "10.0.0.1"},
					{Ipaddress: "192.0.0.1"},
				},
			},
			expected: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeHostName, Address: "host1"},
			},
		},
		{
			config: globalConfig{},
			vm: cloudstack.VirtualMachine{
				Id:       "vm1",
				Hostname: "host1",
				Nic: []cloudstack.Nic{
					{Ipaddress: "10.0.0.1"},
					{Ipaddress: "192.0.0.1"},
				},
			},
			expected: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeHostName, Address: "host1"},
			},
		},
		{
			config: globalConfig{},
			vm: cloudstack.VirtualMachine{
				Id: "vm1",
				Nic: []cloudstack.Nic{
					{Ipaddress: "127.0.0.1"},
				},
			},
			expected: func() []v1.NodeAddress {
				addrs, _ := net.LookupAddr("127.0.0.1")
				result := []v1.NodeAddress{
					{Type: v1.NodeInternalIP, Address: "127.0.0.1"},
				}
				for _, addr := range addrs {
					result = append(result, v1.NodeAddress{Type: v1.NodeHostName, Address: addr})
				}
				return result
			}(),
		},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			srv := cloudstackFake.NewCloudstackServer()
			defer srv.Close()
			csCloud, err := newCSCloud(&CSConfig{
				Global: tt.config,
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
			require.NoError(t, err)
			addrs, err := csCloud.nodeAddresses(&tt.vm)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, addrs)
		})
	}
}
