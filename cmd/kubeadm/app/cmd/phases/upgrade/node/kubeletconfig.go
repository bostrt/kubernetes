/*
Copyright 2019 The Kubernetes Authors.

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

package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"k8s.io/klog/v2"

	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmapiv1 "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/v1beta3"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	cmdutil "k8s.io/kubernetes/cmd/kubeadm/app/cmd/util"
	"k8s.io/kubernetes/cmd/kubeadm/app/constants"
	kubeletphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/kubelet"
	patchnodephase "k8s.io/kubernetes/cmd/kubeadm/app/phases/patchnode"
	"k8s.io/kubernetes/cmd/kubeadm/app/phases/upgrade"
	configutil "k8s.io/kubernetes/cmd/kubeadm/app/util/config"
	dryrunutil "k8s.io/kubernetes/cmd/kubeadm/app/util/dryrun"
)

var (
	kubeletConfigLongDesc = cmdutil.LongDesc(`
		Download the kubelet configuration from a ConfigMap of the form "kubelet-config-1.X" in the cluster,
		where X is the minor version of the kubelet. kubeadm uses the KuberneteVersion field in the kubeadm-config
		ConfigMap to determine what the _desired_ kubelet version is.
		`)
)

// NewKubeletConfigPhase creates a kubeadm workflow phase that implements handling of kubelet-config upgrade.
func NewKubeletConfigPhase() workflow.Phase {
	phase := workflow.Phase{
		Name:  "kubelet-config",
		Short: "Upgrade the kubelet configuration for this node",
		Long:  kubeletConfigLongDesc,
		Run:   runKubeletConfigPhase(),
		InheritFlags: []string{
			options.DryRun,
			options.KubeconfigPath,
		},
	}
	return phase
}

func runKubeletConfigPhase() func(c workflow.RunData) error {
	return func(c workflow.RunData) error {
		data, ok := c.(Data)
		if !ok {
			return errors.New("kubelet-config phase invoked with an invalid data struct")
		}

		// otherwise, retrieve all the info required for kubelet config upgrade
		cfg := data.Cfg()
		dryRun := data.DryRun()

		// Set up the kubelet directory to use. If dry-running, this will return a fake directory
		kubeletDir, err := upgrade.GetKubeletDir(dryRun)
		if err != nil {
			return err
		}

		// TODO: Checkpoint the current configuration first so that if something goes wrong it can be recovered

		// Store the kubelet component configuration.
		if err = kubeletphase.WriteConfigToDisk(&cfg.ClusterConfiguration, kubeletDir); err != nil {
			return err
		}

		// If we're dry-running, print the generated manifests
		if dryRun {
			if err := printFilesIfDryRunning(dryRun, kubeletDir); err != nil {
				return errors.Wrap(err, "error printing files on dryrun")
			}
			return nil
		}

		// Handle a missing URL scheme in the Node CRI socket.
		// Older versions of kubeadm tolerate CRI sockets without URL schemes (/var/run/foo without unix://).
		// During "upgrade node" for worker nodes the cfg.NodeRegistration would be left empty.
		// This requires to call GetNodeRegistration on demand and fetch the node name and CRI socket.
		// If the NodeRegistration (nro) contains a socket without a URL scheme, update it.
		//
		// TODO: this workaround can be removed in 1.25 once all user node sockets have a URL scheme:
		// https://github.com/kubernetes/kubeadm/issues/2426
		var nro *kubeadmapi.NodeRegistrationOptions
		var missingURLScheme bool
		if !dryRun {
			if err := configutil.GetNodeRegistration(data.KubeConfigPath(), data.Client(), nro); err != nil {
				return errors.Wrap(err, "could not retrieve the node registration options for this node")
			}
			missingURLScheme = strings.HasPrefix(nro.CRISocket, kubeadmapiv1.DefaultContainerRuntimeURLScheme)
		}
		if missingURLScheme {
			if !dryRun {
				newSocket := kubeadmapiv1.DefaultContainerRuntimeURLScheme + "://" + nro.CRISocket
				klog.V(2).Infof("ensuring that Node %q has a CRI socket annotation with URL scheme %q", nro.Name, newSocket)
				if err := patchnodephase.AnnotateCRISocket(data.Client(), nro.Name, newSocket); err != nil {
					return errors.Wrapf(err, "error updating the CRI socket for Node %q", nro.Name)
				}
			} else {
				fmt.Println("[dryrun] would update the node CRI socket path to include an URL scheme")
			}
		}

		// TODO: Temporary workaround. Remove in 1.25:
		// https://github.com/kubernetes/kubeadm/issues/2426
		if err := upgrade.UpdateKubeletDynamicEnvFileWithURLScheme(dryRun); err != nil {
			return err
		}

		fmt.Println("[upgrade] The configuration for this node was successfully updated!")
		fmt.Println("[upgrade] Now you should go ahead and upgrade the kubelet package using your package manager.")
		return nil
	}
}

// printFilesIfDryRunning prints the Static Pod manifests to stdout and informs about the temporary directory to go and lookup
func printFilesIfDryRunning(dryRun bool, kubeletDir string) error {
	if !dryRun {
		return nil
	}

	// Print the contents of the upgraded file and pretend like they were in kubeadmconstants.KubeletRunDirectory
	fileToPrint := dryrunutil.FileToPrint{
		RealPath:  filepath.Join(kubeletDir, constants.KubeletConfigurationFileName),
		PrintPath: filepath.Join(constants.KubeletRunDirectory, constants.KubeletConfigurationFileName),
	}
	return dryrunutil.PrintDryRunFiles([]dryrunutil.FileToPrint{fileToPrint}, os.Stdout)
}
