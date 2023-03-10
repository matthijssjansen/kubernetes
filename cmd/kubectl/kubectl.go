/*
Copyright 2014 The Kubernetes Authors.

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

package main

import (
	"fmt"
	"time"

	"k8s.io/component-base/cli"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/cmd"
	"k8s.io/kubectl/pkg/cmd/util"

	// Import to initialize client auth plugins.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

func main() {
	fmt.Println(time.Now().UnixNano(), "[CONTINUUM] 0400 - KUBECTL START")
	klog.V(1).Infoln("kubectl command headers turned off")
	command := cmd.NewDefaultKubectlCommand()
	fmt.Println(time.Now().UnixNano(), "[CONTINUUM] 0402 - KUBECTL COMMAND FORMED")
	if err := cli.RunNoErrOutput(command); err != nil {
		// Pretty-print the error and exit with an error.
		util.CheckErr(err)
	}
	fmt.Println(time.Now().UnixNano(), "[CONTINUUM] 0410 - KUBECTL FINISHED")
}
