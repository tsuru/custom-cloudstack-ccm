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
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cloudstackFake "github.com/tsuru/custom-cloudstack-ccm/cloudstack/fake"
)

func TestReadConfig(t *testing.T) {
	_, err := readConfig(nil)
	if err != nil {
		t.Fatalf("Should not return an error when no config is provided: %v", err)
	}

	cfg, err := readConfig(strings.NewReader(`
 [global]
 project-id-label = tsuru.io/project-id
 service-label = tsuru.io/app-pool
 node-label = tsuru.io/pool
 node-name-label = tsuru.io/iaas-id
 environment-label = tsuru.io/datacenter
 
 [environment "prod"]
 api-url				= https://cloudstack.prod.url
 api-key				= prod-api-key
 secret-key				= prod-secret-key
 ssl-no-verify			= false
 lb-environment-id 		= 999
 lb-domain 				= cs-router.com

 [environment "dev"]
 api-url				= https://cloudstack.dev.url
 api-key				= dev-api-key
 secret-key				= dev-secret-key
 ssl-no-verify			= true
 lb-environment-id 		= 100
 lb-domain 				= cs-router.dev.com

 [custom-command]
 associate-ip = acquireIP
 assign-networks = assignNetworks

 [custom-command-args "acquireIP"]
 a = b
 c = d

 `))
	if err != nil {
		t.Fatalf("Should succeed when a valid config is provided: %v", err)
	}

	if cfg.Environment["prod"].APIURL != "https://cloudstack.prod.url" {
		t.Errorf("incorrect api-url: %s", cfg.Environment["prod"].APIURL)
	}
	if cfg.Environment["prod"].APIKey != "prod-api-key" {
		t.Errorf("incorrect api-key: %s", cfg.Environment["prod"].APIKey)
	}
	if cfg.Environment["prod"].SecretKey != "prod-secret-key" {
		t.Errorf("incorrect secret-key: %s", cfg.Environment["prod"].SecretKey)
	}
	if cfg.Environment["prod"].SSLNoVerify {
		t.Errorf("incorrect ssl-no-verify: %t", cfg.Environment["prod"].SSLNoVerify)
	}
	if cfg.Environment["prod"].LBEnvironmentID != "999" {
		t.Errorf("incorrect lb-environment-id: %s", cfg.Environment["prod"].LBEnvironmentID)
	}
	if cfg.Environment["prod"].LBDomain != "cs-router.com" {
		t.Errorf("incorrect lb-domain: %s", cfg.Environment["prod"].LBDomain)
	}

	if cfg.Environment["dev"].APIURL != "https://cloudstack.dev.url" {
		t.Errorf("incorrect api-url: %s", cfg.Environment["dev"].APIURL)
	}
	if cfg.Environment["dev"].APIKey != "dev-api-key" {
		t.Errorf("incorrect api-key: %s", cfg.Environment["dev"].APIKey)
	}
	if cfg.Environment["dev"].SecretKey != "dev-secret-key" {
		t.Errorf("incorrect secret-key: %s", cfg.Environment["dev"].SecretKey)
	}
	if !cfg.Environment["dev"].SSLNoVerify {
		t.Errorf("incorrect ssl-no-verify: %t", cfg.Environment["dev"].SSLNoVerify)
	}
	if cfg.Environment["dev"].LBEnvironmentID != "100" {
		t.Errorf("incorrect lb-environment-id: %s", cfg.Environment["dev"].LBEnvironmentID)
	}
	if cfg.Environment["dev"].LBDomain != "cs-router.dev.com" {
		t.Errorf("incorrect lb-domain: %s", cfg.Environment["dev"].LBDomain)
	}

	if cfg.Global.ServiceFilterLabel != "tsuru.io/app-pool" {
		t.Errorf("incorrect service-label: %s", cfg.Global.ServiceFilterLabel)
	}
	if cfg.Global.NodeFilterLabel != "tsuru.io/pool" {
		t.Errorf("incorrect node-label: %s", cfg.Global.NodeFilterLabel)
	}
	if cfg.Global.NodeNameLabel != "tsuru.io/iaas-id" {
		t.Errorf("incorrect node-name-label: %s", cfg.Global.NodeNameLabel)
	}
	if cfg.Global.EnvironmentLabel != "tsuru.io/datacenter" {
		t.Errorf("incorrect environment-label: %s", cfg.Global.EnvironmentLabel)
	}
	if cfg.Command.AssociateIP != "acquireIP" {
		t.Errorf("incorrect associate-ip: %s", cfg.Command.AssociateIP)
	}
	if cfg.Command.AssignNetworks != "assignNetworks" {
		t.Errorf("incorrect assign-networks: %s", cfg.Command.AssignNetworks)
	}
	if cfg.CommandArgs == nil {
		t.Errorf("unexpected nil CommandArgs")
	}
	invalidResultMap := cfg.CommandArgs["invalid-entry"].ToMap()
	if !reflect.DeepEqual(invalidResultMap, map[string]string{}) {
		t.Errorf("incorrect value for invalid entry: %#v", invalidResultMap)
	}
	resultMap := cfg.CommandArgs["acquireIP"].ToMap()
	argsEqual := reflect.DeepEqual(resultMap, map[string]string{
		"a": "b",
		"c": "d",
	})
	if !argsEqual {
		t.Errorf("incorrect value for acquireIP command args: %#v", resultMap)
	}
}

func TestReadConfigFallbackSecretsToEnvs(t *testing.T) {
	_, err := readConfig(nil)
	if err != nil {
		t.Fatalf("Should not return an error when no config is provided: %v", err)
	}
	os.Setenv("CLOUDSTACK_PROD_API_URL", "https://cloudstack.url")
	os.Setenv("CLOUDSTACK_PROD_API_KEY", "a-valid-api-key")
	os.Setenv("CLOUDSTACK_PROD_SECRET_KEY", "a-valid-secret-key")
	defer os.Unsetenv("CLOUDSTACK_PROD_API_URL")
	defer os.Unsetenv("CLOUDSTACK_PROD_API_KEY")
	defer os.Unsetenv("CLOUDSTACK_PROD_SECRET_KEY")

	cfg, err := readConfig(strings.NewReader(`
 [global]
 project-id-label = tsuru.io/project-id
 service-label = tsuru.io/app-pool
 node-label = tsuru.io/pool
 node-name-label = tsuru.io/iaas-id
 environment-label = tsuru.io/datacenter
 
 [environment "prod"]
 ssl-no-verify			= true
 lb-environment-id 		= 999
 lb-domain 				= cs-router.com

 [custom-command]
 associate-ip = acquireIP
 assign-networks = assignNetworks
 `))
	if err != nil {
		t.Fatalf("Should succeed when a valid config is provided: %v", err)
	}

	if cfg.Environment["prod"].APIURL != "https://cloudstack.url" {
		t.Errorf("incorrect api-url: %s", cfg.Environment["prod"].APIURL)
	}
	if cfg.Environment["prod"].APIKey != "a-valid-api-key" {
		t.Errorf("incorrect api-key: %s", cfg.Environment["prod"].APIKey)
	}
	if cfg.Environment["prod"].SecretKey != "a-valid-secret-key" {
		t.Errorf("incorrect secret-key: %s", cfg.Environment["prod"].SecretKey)
	}
	if !cfg.Environment["prod"].SSLNoVerify {
		t.Errorf("incorrect ssl-no-verify: %t", cfg.Environment["prod"].SSLNoVerify)
	}
	if cfg.Environment["prod"].LBEnvironmentID != "999" {
		t.Errorf("incorrect lb-environment-id: %s", cfg.Environment["prod"].LBEnvironmentID)
	}
	if cfg.Environment["prod"].LBDomain != "cs-router.com" {
		t.Errorf("incorrect lb-domain: %s", cfg.Environment["prod"].LBDomain)
	}

	if cfg.Global.ServiceFilterLabel != "tsuru.io/app-pool" {
		t.Errorf("incorrect service-label: %s", cfg.Global.ServiceFilterLabel)
	}
	if cfg.Global.NodeFilterLabel != "tsuru.io/pool" {
		t.Errorf("incorrect node-label: %s", cfg.Global.NodeFilterLabel)
	}
	if cfg.Global.NodeNameLabel != "tsuru.io/iaas-id" {
		t.Errorf("incorrect node-name-label: %s", cfg.Global.NodeNameLabel)
	}
	if cfg.Global.EnvironmentLabel != "tsuru.io/datacenter" {
		t.Errorf("incorrect environment-label: %s", cfg.Global.EnvironmentLabel)
	}
	if cfg.Command.AssociateIP != "acquireIP" {
		t.Errorf("incorrect associate-ip: %s", cfg.Command.AssociateIP)
	}
	if cfg.Command.AssignNetworks != "assignNetworks" {
		t.Errorf("incorrect assign-networks: %s", cfg.Command.AssignNetworks)
	}
}

func Test_newCSCloud(t *testing.T) {
	srv := cloudstackFake.NewCloudstackServer()
	defer srv.Close()
	csCloud, err := newCSCloud(&CSConfig{
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
		},
	})
	require.Nil(t, err)
	assert.Contains(t, csCloud.environments, "env1")
	assert.NotNil(t, csCloud.environments["env1"].manager)
	assert.NotNil(t, csCloud.environments["env1"].client)
}
