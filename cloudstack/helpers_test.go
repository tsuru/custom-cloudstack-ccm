package cloudstack

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cloudstackFake "github.com/tsuru/custom-cloudstack-ccm/cloudstack/fake"
	"github.com/xanzy/go-cloudstack/v2/cloudstack"
)

func Test_newCloudstackManager(t *testing.T) {
	srv := cloudstackFake.NewCloudstackServer()
	defer srv.Close()
	csCli := cloudstack.NewAsyncClient(srv.URL, "a", "b", true)
	manager, err := newCloudstackManager(csCli)
	require.NoError(t, err)
	assert.NotNil(t, manager.client)
	assert.NotNil(t, manager.cache)
}

func Test_cloudstackManager_virtualMachineByName(t *testing.T) {
	srv := cloudstackFake.NewCloudstackServer()
	defer srv.Close()
	csCli := cloudstack.NewAsyncClient(srv.URL, "a", "b", true)
	manager, err := newCloudstackManager(csCli)
	require.NoError(t, err)
	vm1, err := manager.virtualMachineByName("n1", "proj1")
	require.NoError(t, err)
	assert.Equal(t, vm1.Name, "n1")
	srv.HasCalls(t, []cloudstackFake.MockAPICall{
		{Command: "listVirtualMachines", Params: url.Values{"name": []string{"n1"}, "projectid": []string{"proj1"}}},
	})
	for i := 0; i < 100; i++ {
		vm2, err := manager.virtualMachineByName("n1", "proj1")
		require.NoError(t, err)
		assert.Equal(t, vm1, vm2)
	}
	assert.Less(t, len(srv.Calls), 100)
}
