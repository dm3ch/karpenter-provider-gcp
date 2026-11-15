/*
Copyright 2026 The CloudPilot AI Authors.

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

package metadata

import (
	"testing"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
)

var (
	benchmarkParsedMetadata   *InstanceMetadata
	benchmarkRenderedMetadata *compute.Metadata
)

func benchSourceMetadata() *compute.Metadata {
	return &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeLabelsKey, Value: lo.ToPtr("source=true")},
		{Key: KubeEnvKey, Value: lo.ToPtr("CLUSTER_NAME: benchmark\nDNS_SERVER_IP: 10.0.0.10\nCUSTOM_ENTRY: first line\n  continued-line\nKUBELET_ARGS: --v=2 --cloud-provider=external --max-pods=110 --node-labels=source=true --register-with-taints=dedicated=gpu:NoSchedule\n")},
		{Key: KubeletConfigKey, Value: lo.ToPtr("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nnodeStatusUpdateFrequency: 10s\nmaxPods: 110\ncpuManagerPolicy: none\n")},
		{Key: "cluster-name", Value: lo.ToPtr("benchmark")},
		{Key: "configure-sh", Value: lo.ToPtr("#!/bin/bash\necho synthetic-bootstrap\n")},
	}}
}

func BenchmarkFromSourceTemplate(b *testing.B) {
	source := benchSourceMetadata()
	model, err := FromSourceTemplate(source)
	if err != nil {
		b.Fatal(err)
	}
	if value, ok := model.GetKubeEnvEntry("CUSTOM_ENTRY"); !ok || value != "first line\n  continued-line" {
		b.Fatal("source kube-env was not preserved")
	}
	b.ReportAllocs()
	for b.Loop() {
		model, err = FromSourceTemplate(source)
		if err != nil {
			b.Fatal(err)
		}
	}
	benchmarkParsedMetadata = model
}

func BenchmarkToComputeMetadata(b *testing.B) {
	model, err := FromSourceTemplate(benchSourceMetadata())
	if err != nil {
		b.Fatal(err)
	}
	model.SetKubeLabels(Labels{"karpenter.sh/nodepool": "benchmark"})
	model.SetRegistrationNodeLabels(Labels{"karpenter.sh/nodepool": "benchmark"})
	model.SetKubeletMaxPods(110)
	model.AppendNodeTaint(corev1.Taint{Key: "karpenter.sh/unregistered", Effect: corev1.TaintEffectNoExecute})
	result, err := model.ToComputeMetadata()
	if err != nil {
		b.Fatal(err)
	}
	if metadataValuesFromCompute(result)["cluster-name"] != "benchmark" {
		b.Fatal("source cluster metadata was not rendered")
	}
	b.ReportAllocs()
	for b.Loop() {
		result, err = model.ToComputeMetadata()
		if err != nil {
			b.Fatal(err)
		}
	}
	benchmarkRenderedMetadata = result
}
