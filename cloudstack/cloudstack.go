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
	"errors"
	"fmt"
	"io"

	"github.com/xanzy/go-cloudstack/cloudstack"
	"gopkg.in/gcfg.v1"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/controller"
)

// ProviderName is the name of this cloud provider.
const ProviderName = "custom-cloudstack"

// CSConfig wraps the config for the CloudStack cloud provider.
type CSConfig struct {
	Global struct {
		APIURL             string `gcfg:"api-url"`
		APIKey             string `gcfg:"api-key"`
		SecretKey          string `gcfg:"secret-key"`
		SSLNoVerify        bool   `gcfg:"ssl-no-verify"`
		ProjectID          string `gcfg:"project-id"`
		Zone               string `gcfg:"zone"`
		ServiceFilterLabel string `gcfg:"service-label"`
		NodeFilterLabel    string `gcfg:"node-label"`
		NodeNameLabel      string `gcfg:"node-name-label"`
	}
	Command struct {
		AssociateIP    string `gcfg:"associate-ip"`
		AssignNetworks string `gcfg:"assign-networks"`
	} `gcfg:"custom-command"`
}

// CSCloud is an implementation of Interface for CloudStack.
type CSCloud struct {
	client    *cloudstack.CloudStackClient
	projectID string // If non-"", all resources will be created within this project
	zone      string

	// Labels used to match services to nodes
	serviceLabel string
	nodeLabel    string

	// Node label that contains the virtual machine name used to match with cloudstack
	nodeNameLabel string

	// Custom command to be used to associate an IP to a LB
	customAssociateIPCommand string

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

	return cfg, nil
}

// newCSCloud creates a new instance of CSCloud.
func newCSCloud(cfg *CSConfig) (*CSCloud, error) {
	cs := &CSCloud{
		projectID:                cfg.Global.ProjectID,
		zone:                     cfg.Global.Zone,
		serviceLabel:             cfg.Global.ServiceFilterLabel,
		nodeLabel:                cfg.Global.NodeFilterLabel,
		nodeNameLabel:            cfg.Global.NodeNameLabel,
		customAssociateIPCommand: cfg.Command.AssociateIP,
	}

	if cfg.Global.APIURL != "" && cfg.Global.APIKey != "" && cfg.Global.SecretKey != "" {
		cs.client = cloudstack.NewAsyncClient(cfg.Global.APIURL, cfg.Global.APIKey, cfg.Global.SecretKey, !cfg.Global.SSLNoVerify)
	}

	if cs.client == nil {
		return nil, errors.New("no cloud provider config given")
	}

	return cs, nil
}

// Initialize passes a Kubernetes clientBuilder interface to the cloud provider
func (cs *CSCloud) Initialize(clientBuilder controller.ControllerClientBuilder) {}

// LoadBalancer returns an implementation of LoadBalancer for CloudStack.
func (cs *CSCloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	if cs.client == nil {
		return nil, false
	}

	return cs, true
}

// Instances returns an implementation of Instances for CloudStack.
func (cs *CSCloud) Instances() (cloudprovider.Instances, bool) {
	return nil, false
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
