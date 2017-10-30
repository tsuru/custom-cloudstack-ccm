/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudstack

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/xanzy/go-cloudstack/cloudstack"
	"gopkg.in/gcfg.v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/controller"
)

// ProviderName is the name of this cloud provider.
const ProviderName = "custom-cloudstack"

// CSConfig wraps the config for the CloudStack cloud provider.
type CSConfig struct {
	Global struct {
		ServiceFilterLabel string `gcfg:"service-label"`
		NodeFilterLabel    string `gcfg:"node-label"`
		NodeNameLabel      string `gcfg:"node-name-label"`
		ProjectIDLabel     string `gcfg:"project-id-label"`
		EnvironmentLabel   string `gcfg:"environment-label"`
	} `gcfg:"global"`
	Environment map[string]*struct {
		APIURL          string `gcfg:"api-url"`
		APIKey          string `gcfg:"api-key"`
		SecretKey       string `gcfg:"secret-key"`
		SSLNoVerify     bool   `gcfg:"ssl-no-verify"`
		LBEnvironmentID string `gcfg:"lb-environment-id"`
		LBDomain        string `gcfg:"lb-domain"`
	} `gcfg:"environment"`
	Command struct {
		AssociateIP    string `gcfg:"associate-ip"`
		DisassociateIP string `gcfg:"disassociate-ip"`
		AssignNetworks string `gcfg:"assign-networks"`
	} `gcfg:"custom-command"`
}

type CSEnvironment struct {
	client          *cloudstack.CloudStackClient
	lbEnvironmentID string
	lbDomain        string
}

// CSCloud is an implementation of Interface for CloudStack.
type CSCloud struct {
	environments map[string]CSEnvironment

	// environmentLabel used to figure out the resource CS environment
	environmentLabel string

	kubeClient kubernetes.Interface

	// Labels used to match services to nodes
	serviceLabel string
	nodeLabel    string

	// Node label that contains the virtual machine name used to match with cloudstack
	nodeNameLabel string

	// label that contains the project ID of the given resource
	projectIDLabel string

	// Custom command to be used to associate an IP to a LB
	customAssociateIPCommand string

	// Custom command to be used to disassociate an IP from a LB
	customDisassociateIPCommand string

	// Custom command to be used to assign multiple networks to a LB
	customAssignNetworksCommand string
}

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		cfg, err := readConfig(config)
		if err != nil {
			return nil, err
		}

		return newCSCloud(cfg)
	})
}

func readConfig(config io.Reader) (*CSConfig, error) {
	cfg := &CSConfig{}

	if config == nil {
		return cfg, nil
	}

	if err := gcfg.ReadInto(cfg, config); err != nil {
		return nil, fmt.Errorf("could not parse cloud provider config: %v", err)
	}

	for k, v := range cfg.Environment {
		if v.APIURL == "" {
			v.APIURL = os.Getenv("CLOUDSTACK_" + strings.ToUpper(k) + "_API_URL")
		}
		if v.APIKey == "" {
			v.APIKey = os.Getenv("CLOUDSTACK_" + strings.ToUpper(k) + "_API_KEY")
		}
		if v.SecretKey == "" {
			v.SecretKey = os.Getenv("CLOUDSTACK_" + strings.ToUpper(k) + "_SECRET_KEY")
		}
	}

	return cfg, nil
}

// newCSCloud creates a new instance of CSCloud.
func newCSCloud(cfg *CSConfig) (*CSCloud, error) {
	cs := &CSCloud{
		serviceLabel:                cfg.Global.ServiceFilterLabel,
		nodeLabel:                   cfg.Global.NodeFilterLabel,
		nodeNameLabel:               cfg.Global.NodeNameLabel,
		projectIDLabel:              cfg.Global.ProjectIDLabel,
		environmentLabel:            cfg.Global.EnvironmentLabel,
		customAssociateIPCommand:    cfg.Command.AssociateIP,
		customAssignNetworksCommand: cfg.Command.AssignNetworks,
		customDisassociateIPCommand: cfg.Command.DisassociateIP,
	}

	for k, v := range cfg.Environment {
		if v.APIURL == "" || v.APIKey == "" || v.SecretKey == "" {
			return nil, fmt.Errorf("missing credentials for environment %q", k)
		}
		cs.environments[k] = CSEnvironment{
			lbEnvironmentID: v.LBEnvironmentID,
			lbDomain:        v.LBDomain,
			client:          cloudstack.NewAsyncClient(v.APIURL, v.APIKey, v.SecretKey, !v.SSLNoVerify),
		}
	}

	return cs, nil
}

// Initialize passes a Kubernetes clientBuilder interface to the cloud provider
func (cs *CSCloud) Initialize(clientBuilder controller.ControllerClientBuilder) {
	cs.kubeClient = clientBuilder.ClientGoClientOrDie(ProviderName)
}

// LoadBalancer returns an implementation of LoadBalancer for CloudStack.
func (cs *CSCloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	if len(cs.environments) == 0 {
		return nil, false
	}
	return cs, true
}

// Instances returns an implementation of Instances for CloudStack.
func (cs *CSCloud) Instances() (cloudprovider.Instances, bool) {
	if len(cs.environments) == 0 {
		return nil, false
	}
	return cs, true
}

// Zones returns an implementation of Zones for CloudStack.
func (cs *CSCloud) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

// Clusters returns an implementation of Clusters for CloudStack.
func (cs *CSCloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes returns an implementation of Routes for CloudStack.
func (cs *CSCloud) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (cs *CSCloud) ProviderName() string {
	return ProviderName
}

// ScrubDNS filters DNS settings for pods.
func (cs *CSCloud) ScrubDNS(nameservers, searches []string) (nsOut, srchOut []string) {
	return nameservers, searches
}

// HasClusterID returns true if the cluster has a clusterID
func (cs *CSCloud) HasClusterID() bool {
	return true
}
