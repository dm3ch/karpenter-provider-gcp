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

func benchSourceMetadata() *compute.Metadata {
	return &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeLabelsKey, Value: lo.ToPtr("cloud.google.com/gke-nodepool=default,cloud.google.com/gke-os-distribution=cos")},
		{Key: KubeEnvKey, Value: lo.ToPtr("CLUSTER_NAME: test\nDNS_SERVER_IP: 10.0.0.10\nENABLE_NODE_PROBLEM_DETECTOR: standalone\nCA_CERT: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t\n  continued-line\nKUBELET_ARGS: --v=2 --cloud-provider=external --max-pods=110 --node-labels=cloud.google.com/gke-nodepool=default,cloud.google.com/gke-os-distribution=cos --register-with-taints=dedicated=gpu:NoSchedule\nNODE_PROBLEM_DETECTOR_TOKEN: abc\n")},
		{Key: KubeletConfigKey, Value: lo.ToPtr("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nnodeStatusUpdateFrequency: 10s\nmaxPods: 110\ncpuManagerPolicy: none\n")},
		{Key: "cluster-name", Value: lo.ToPtr("test")},
		{Key: "cluster-location", Value: lo.ToPtr("us-central1")},
		{Key: "configure-sh", Value: lo.ToPtr("#!/bin/bash\necho configure\n")},
		{Key: "unowned-source-key", Value: lo.ToPtr("source")},
	}}
}

func BenchmarkFromSourceTemplate(b *testing.B) {
	src := benchSourceMetadata()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := FromSourceTemplate(src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkToComputeMetadata(b *testing.B) {
	model, err := FromSourceTemplate(benchSourceMetadata())
	if err != nil {
		b.Fatal(err)
	}
	model.SetKubeLabels(Labels{"karpenter.sh/nodepool": "default", "karpenter.k8s.gcp/gcenodeclass": "default"})
	model.SetRegistrationNodeLabels(Labels{"karpenter.sh/nodepool": "default"})
	model.SetKubeletMaxPods(110)
	model.AppendNodeTaint(corev1.Taint{Key: "karpenter.sh/unregistered", Effect: corev1.TaintEffectNoExecute})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := model.ToComputeMetadata(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseSourceKubeletArgs(b *testing.B) {
	raw := "--v=2 --cloud-provider=external --max-pods=110 --node-labels=a=b,c=d,e=f --register-with-taints=dedicated=gpu:NoSchedule --experimental-mounter-path=/home/kubernetes/containerized_mounter/mounter"
	for i := 0; i < b.N; i++ {
		_ = ParseSourceKubeletArgs(raw).String()
	}
}
