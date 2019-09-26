// Copyright 2017 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/pflag"
	_ "github.com/tsuru/custom-cloudstack-ccm/cloudstack"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/component-base/logs"
	"k8s.io/kubernetes/cmd/cloud-controller-manager/app"
	_ "k8s.io/kubernetes/pkg/client/metrics/prometheus" // for client metric registration
	_ "k8s.io/kubernetes/pkg/version/prometheus"        // for version metric registration
)

func init() {
	healthz.InstallHandler(http.DefaultServeMux)
}

func main() {
	command := app.NewCloudControllerManagerCommand()

	command.Flags().VisitAll(func(fl *pflag.Flag) {
		var err error
		switch fl.Name {
		case "cloud-provider":
			err = fl.Value.Set("custom-cloudstack")
		}

		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to set flag %q: %s\n", fl.Name, err)
			os.Exit(1)
		}
	})

	logs.InitLogs()
	defer logs.FlushLogs()

	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
