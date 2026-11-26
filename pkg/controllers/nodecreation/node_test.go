/*
Copyright 2025 The CloudPilot AI Authors.

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

package nodecreation

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/instance"
)

func TestFilterNodeLabels(t *testing.T) {
	in := map[string]string{
		// Provider labels and GKE labels outside the reserved namespaces are kept.
		"karpenter.sh/nodepool":                  "default",
		"cloud.google.com/gke-os-distribution":   "cos",
		"iam.gke.io/gke-metadata-server-enabled": "true",
		"max-pods-per-node":                      "110",
		// A trusted component may set node-restriction labels a kubelet cannot.
		instance.GKESecondaryBootDiskLabelPrefix + "image": "CONTAINER_IMAGE_CACHE.project",
		// What a kubelet may set on itself.
		"kubernetes.io/hostname":                 "node-1",
		"topology.kubernetes.io/zone":            "us-central1-a",
		"node.kubernetes.io/instance-type":       "n2-standard-2",
		"node.kubernetes.io/masq-agent-ds-ready": "true",
		"kubelet.kubernetes.io/some-label":       "x",
		// What it may not, so VM metadata cannot be used to grant roles or pretend to be core.
		"node-role.kubernetes.io/control-plane": "",
		"kubernetes.io/metadata.name":           "x",
		"example.k8s.io/anything":               "x",
		"karpenter.sh/registered":               "true",
		"karpenter.sh/initialized":              "true",
	}

	kept, dropped := filterNodeLabels(in)

	require.Equal(t, []string{
		"example.k8s.io/anything",
		"karpenter.sh/initialized",
		"karpenter.sh/registered",
		"kubernetes.io/metadata.name",
		"node-role.kubernetes.io/control-plane",
	}, dropped)
	require.Len(t, kept, len(in)-len(dropped))
	for _, key := range []string{
		"karpenter.sh/nodepool", "cloud.google.com/gke-os-distribution", "iam.gke.io/gke-metadata-server-enabled", "max-pods-per-node",
		instance.GKESecondaryBootDiskLabelPrefix + "image",
		"kubernetes.io/hostname", "topology.kubernetes.io/zone", "node.kubernetes.io/instance-type",
		"node.kubernetes.io/masq-agent-ds-ready", "kubelet.kubernetes.io/some-label",
	} {
		require.Contains(t, kept, key)
	}
}

func TestRenderNodeTaints(t *testing.T) {
	gpu := map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "default"}
	taintsOf := func(nodeClass *v1alpha1.GCENodeClass, labels map[string]string) []string {
		node, _ := renderNode("n", "gce://p/z/n", nodeClass, labels)
		var out []string
		for _, taint := range node.Spec.Taints {
			out = append(out, taint.ToString())
		}
		return out
	}
	base := []string{instance.UnregisteredTaintValue, "node.cloudprovider.kubernetes.io/uninitialized=true:NoSchedule"}

	require.Equal(t, base, taintsOf(&v1alpha1.GCENodeClass{}, map[string]string{}))
	require.Equal(t, append(base, "nvidia.com/gpu=present:NoSchedule"),
		taintsOf(&v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{AutoGPUTaint: true}}, gpu))
}
