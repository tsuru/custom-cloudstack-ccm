package cloudstack

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xanzy/go-cloudstack/cloudstack"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/cloudprovider"
)

func TestCSCloudGetZone(t *testing.T) {
	cs := &CSCloud{
		environments: map[string]CSEnvironment{
			"": CSEnvironment{},
		},
	}
	zones, implemented := cs.Zones()
	require.True(t, implemented)
	zone, err := zones.GetZone()
	require.Nil(t, err)
	assert.Equal(t, cloudprovider.Zone{}, zone)
}

func TestCSCloudGetZoneByNodeName(t *testing.T) {
	kubeCli := fake.NewSimpleClientset()
	_, err := kubeCli.CoreV1().Nodes().Create(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "mynode",
			Labels: map[string]string{"projid": "myproj"},
		},
	})
	require.Nil(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("command")
		if cmd == "listProjects" {
			w.Write([]byte(fmt.Sprintf(`{"%s": {"count": 1, "project": [{"id": "myproj"}]}}`, cmd)))
		} else if cmd == "listVirtualMachines" {
			w.Write([]byte(fmt.Sprintf(`{"%s": {"count": 1, "virtualmachine": [{"id": "machineid1", "name": "mynode", "zonename": "myzone"}]}}`, cmd)))
		}
	}))
	defer srv.Close()
	cs := &CSCloud{
		kubeClient: kubeCli,
		config: CSConfig{
			Global: globalConfig{
				ProjectIDLabel: "projid",
			},
		},
		environments: map[string]CSEnvironment{
			"": CSEnvironment{
				client: cloudstack.NewAsyncClient(srv.URL, "", "", true),
			},
		},
	}
	zones, implemented := cs.Zones()
	require.True(t, implemented)
	zone, err := zones.GetZoneByNodeName("mynode")
	require.Nil(t, err)
	assert.Equal(t, cloudprovider.Zone{
		FailureDomain: "myzone",
		Region:        "myzone",
	}, zone)
}

func TestCSCloudGetZoneByProviderID(t *testing.T) {
	kubeCli := fake.NewSimpleClientset()
	_, err := kubeCli.CoreV1().Nodes().Create(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "mynode",
			Labels: map[string]string{"projid": "myproj", "nodename": "id123"},
		},
	})
	require.Nil(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("command")
		if cmd == "listProjects" {
			w.Write([]byte(fmt.Sprintf(`{"%s": {"count": 1, "project": [{"id": "myproj"}]}}`, cmd)))
		} else if cmd == "listVirtualMachines" {
			w.Write([]byte(fmt.Sprintf(`{"%s": {"count": 1, "virtualmachine": [{"id": "machineid1", "name": "mynode", "zonename": "myzone"}]}}`, cmd)))
		}
	}))
	defer srv.Close()
	cs := &CSCloud{
		kubeClient: kubeCli,
		config: CSConfig{
			Global: globalConfig{
				ProjectIDLabel: "projid",
				NodeNameLabel:  "nodename",
			},
		},
		environments: map[string]CSEnvironment{
			"": CSEnvironment{
				client: cloudstack.NewAsyncClient(srv.URL, "", "", true),
			},
		},
	}
	zones, implemented := cs.Zones()
	require.True(t, implemented)
	zone, err := zones.GetZoneByProviderID("custom-cloudstack:///myproj/id123")
	require.Nil(t, err)
	assert.Equal(t, cloudprovider.Zone{
		FailureDomain: "myzone",
		Region:        "myzone",
	}, zone)
}
