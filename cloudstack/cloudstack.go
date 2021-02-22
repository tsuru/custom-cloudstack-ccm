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
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/xanzy/go-cloudstack/v2/cloudstack"
	"gopkg.in/gcfg.v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/transport"
	cloudprovider "k8s.io/cloud-provider"
)

// ProviderName is the name of this cloud provider.
const ProviderName = "custom-cloudstack"

var (
	asyncJobWaitTimeout = int64((5 * time.Minute).Seconds())
)

// CSConfig wraps the config for the CloudStack cloud provider.
type CSConfig struct {
	Global      globalConfig                  `gcfg:"global"`
	Environment map[string]*environmentConfig `gcfg:"environment"`
	Command     commandConfig                 `gcfg:"custom-command"`
	CommandArgs map[string]*commandArgsConfig `gcfg:"custom-command-args"`
}

type globalConfig struct {
	ServiceFilterLabel string `gcfg:"service-label"`
	NodeFilterLabel    string `gcfg:"node-label"`
	NodeNameLabel      string `gcfg:"node-name-label"`
	ProjectIDLabel     string `gcfg:"project-id-label"`
	EnvironmentLabel   string `gcfg:"environment-label"`
	InternalIPIndex    int    `gcfg:"internal-ip-index"`
	ExternalIPIndex    int    `gcfg:"external-ip-index"`
	UpdateLBWorkers    int    `gcfg:"update-lb-workers"`
}

type environmentConfig struct {
	APIURL          string `gcfg:"api-url"`
	APIKey          string `gcfg:"api-key"`
	SecretKey       string `gcfg:"secret-key"`
	LBEnvironmentID string `gcfg:"lb-environment-id"`
	LBDomain        string `gcfg:"lb-domain"`
	ProjectID       string `gcfg:"project-id"`
	SSLNoVerify     bool   `gcfg:"ssl-no-verify"`
	RemoveLBs       bool   `gcfg:"remove-lbs-on-delete"`
}

type commandConfig struct {
	AssociateIP    string `gcfg:"associate-ip"`
	DisassociateIP string `gcfg:"disassociate-ip"`
	AssignNetworks string `gcfg:"assign-networks"`
	DeleteLBRule   string `gcfg:"delete-lb-rule"`
}

type commandArgsConfig struct {
	gcfg.Idxer
	Vals map[gcfg.Idx]*string
}

func (c *commandArgsConfig) ToMap() map[string]string {
	result := map[string]string{}
	if c == nil {
		return result
	}
	keys := c.Names()
	for _, k := range keys {
		value := c.Vals[c.Idx(k)]
		if value != nil {
			result[k] = *value
		}
	}
	return result
}

type CSEnvironment struct {
	client          *cloudstack.CloudStackClient
	manager         *cloudstackManager
	lbEnvironmentID string
	lbDomain        string
	// Indicates if LBs should be deleted upon service removal
	removeLBs bool
}

// CSCloud is an implementation of Interface for CloudStack.
type CSCloud struct {
	environments  map[string]CSEnvironment
	kubeClient    kubernetes.Interface
	recorder      record.EventRecorder
	updateLBQueue *serviceNodeQueue
	config        CSConfig

	// Lock used to prevent parallel calls to UpdateLoadBalancer and
	// EnsureLoadBalancer. See kubernetes/kubernetes#53462 (closed but not
	// solved) and kubernetes/kubernetes#55336 (this last one was reverted as
	// of kubernetes 1.14)
	svcLock *serviceLock
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
		environments: make(map[string]CSEnvironment),
		svcLock:      &serviceLock{},
		config:       *cfg,
	}

	for k, v := range cfg.Environment {
		if v.APIURL == "" || v.APIKey == "" || v.SecretKey == "" {
			return nil, fmt.Errorf("missing credentials for environment %q", k)
		}
		baseTransport := &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: v.SSLNoVerify},
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		opts := []cloudstack.ClientOption{
			cloudstack.WithAsyncTimeout(asyncJobWaitTimeout),
			cloudstack.WithHTTPClient(&http.Client{
				Transport: transport.DebugWrappers(baseTransport),
				Timeout:   60 * time.Second,
			}),
		}
		csCli := cloudstack.NewAsyncClient(v.APIURL, v.APIKey, v.SecretKey, !v.SSLNoVerify, opts...)
		manager, err := newCloudstackManager(csCli)
		if err != nil {
			return nil, err
		}
		cs.environments[k] = CSEnvironment{
			lbEnvironmentID: v.LBEnvironmentID,
			lbDomain:        v.LBDomain,
			client:          csCli,
			manager:         manager,
			removeLBs:       v.RemoveLBs,
		}
	}

	return cs, nil
}

// Initialize passes a Kubernetes clientBuilder interface to the cloud provider
func (cs *CSCloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	cs.kubeClient = clientBuilder.ClientOrDie(ProviderName)
	b := record.NewBroadcaster()
	b.StartRecordingToSink(&typedcore.EventSinkImpl{Interface: typedcore.New(cs.kubeClient.CoreV1().RESTClient()).Events("")})
	cs.recorder = b.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "csccm"})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	cs.updateLBQueue = cs.startLBUpdateQueue(ctx)
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
	if len(cs.environments) == 0 {
		return nil, false
	}
	return cs, true
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

// HasClusterID returns true if the cluster has a clusterID
func (cs *CSCloud) HasClusterID() bool {
	return true
}
