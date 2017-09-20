// Copyright 2017 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"github.com/tsuru/custom-cloudstack-ccm/controller"
)

func main() {
	dc := &controller.DummyController{}
	dc.Start()
}
