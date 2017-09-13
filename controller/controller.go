// Copyright 2017 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package controller

import (
	"log"
	"net/http"
	"os/exec"
	"strings"

	"github.com/spf13/pflag"
	api "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/ingress/core/pkg/ingress"
	k8sctl "k8s.io/ingress/core/pkg/ingress/controller"
	"k8s.io/ingress/core/pkg/ingress/defaults"
)

type DummyController struct{}

func (dc DummyController) Start() {
	ic := k8sctl.NewIngressController(dc)
	defer func() {
		log.Printf("Shutting down ingress controller...")
		ic.Stop()
	}()
	ic.Start()
}

func (dc DummyController) SetConfig(cfgMap *api.ConfigMap) {
	log.Printf("Config map %+v", cfgMap)
}

func (dc DummyController) Test(file string) *exec.Cmd {
	return exec.Command("echo", file)
}

func (dc DummyController) OnUpdate(updatePayload ingress.Configuration) error {
	log.Printf("Received OnUpdate notification")
	for _, b := range updatePayload.Backends {
		eps := []string{}
		for _, e := range b.Endpoints {
			eps = append(eps, e.Address)
		}
		log.Printf("%v: %v", b.Name, strings.Join(eps, ", "))
	}

	log.Printf("Reloaded new config")
	return nil
}

func (dc DummyController) BackendDefaults() defaults.Backend {
	return defaults.Backend{}
}

func (n DummyController) Name() string {
	return "dummy Controller"
}

func (n DummyController) Check(_ *http.Request) error {
	return nil
}

func (dc DummyController) Info() *ingress.BackendInfo {
	return &ingress.BackendInfo{
		Name:       "dummy",
		Release:    "0.0.0",
		Build:      "git-00000000",
		Repository: "git://foo.bar.com",
	}
}

func (n DummyController) ConfigureFlags(*pflag.FlagSet) {
}

func (n DummyController) OverrideFlags(*pflag.FlagSet) {
}

func (n DummyController) SetListers(lister ingress.StoreLister) {

}

func (n DummyController) DefaultIngressClass() string {
	return "dummy"
}

func (n DummyController) UpdateIngressStatus(*extensions.Ingress) []api.LoadBalancerIngress {
	return nil
}

// DefaultEndpoint returns the default endpoint to be use as default server that returns 404.
func (n DummyController) DefaultEndpoint() ingress.Endpoint {
	return ingress.Endpoint{
		Address: "127.0.0.1",
		Port:    "8181",
		Target:  &api.ObjectReference{},
	}
}
