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

package instance

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestNodeRegistrationTaints(t *testing.T) {
	gpuLabels := map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "default"}
	autoTaint := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{AutoGPUTaint: true}}

	tests := []struct {
		name       string
		nodeClass  *v1alpha1.GCENodeClass
		kubeLabels map[string]string
		wantExtra  []string
	}{
		{name: "cpu node", nodeClass: &v1alpha1.GCENodeClass{}},
		{name: "cpu node ignores autoGPUTaint", nodeClass: autoTaint},
		{name: "gpu node without autoGPUTaint", nodeClass: &v1alpha1.GCENodeClass{}, kubeLabels: gpuLabels},
		{name: "gpu node with autoGPUTaint", nodeClass: autoTaint, kubeLabels: gpuLabels, wantExtra: []string{gpuTaintValue}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, taint := range NodeRegistrationTaints(tt.nodeClass, tt.kubeLabels) {
				got = append(got, taint.ToString())
			}
			// The first taint must be exactly the one written into kube-env at launch,
			// or core registration would not recognize and remove it.
			want := append([]string{UnregisteredTaintValue, "node.cloudprovider.kubernetes.io/uninitialized=true:NoSchedule"}, tt.wantExtra...)
			require.Equal(t, want, got)
		})
	}
}

func TestLaunchedKubeLabelsReadsOnlyTheKubeLabelsItem(t *testing.T) {
	gce := func(items ...*compute.MetadataItems) *compute.Instance {
		return &compute.Instance{Name: "node-1", Metadata: &compute.Metadata{Items: items}}
	}

	require.Equal(t, map[string]string{"karpenter.sh/nodepool": "default", "karpenter.sh/registered": "true"}, launchedKubeLabels(gce(
		&compute.MetadataItems{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --node-labels=a=b\nSECRET_TOKEN: s3cret\n")},
		&compute.MetadataItems{Key: "kube-labels", Value: lo.ToPtr("karpenter.sh/nodepool=default,karpenter.sh/registered=true")},
	)), "only the kube-labels item is read; kube-env is never retained")
	require.Nil(t, launchedKubeLabels(gce()), "an absent item is not an empty set")
	require.Nil(t, launchedKubeLabels(&compute.Instance{}))
}
