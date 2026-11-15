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

package instancetype

import (
	"context"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
)

func benchMachineTypes() []*computepb.MachineType {
	return []*computepb.MachineType{
		{Name: lo.ToPtr("e2-standard-4"), GuestCpus: lo.ToPtr[int32](4), MemoryMb: lo.ToPtr[int32](16384)},
		{Name: lo.ToPtr("n2-standard-8"), GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](32768),
			BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr[int32](2)}},
		{Name: lo.ToPtr("c4-highmem-32"), GuestCpus: lo.ToPtr[int32](32), MemoryMb: lo.ToPtr[int32](253952)},
		{Name: lo.ToPtr("t2a-standard-16"), GuestCpus: lo.ToPtr[int32](16), MemoryMb: lo.ToPtr[int32](65536), Architecture: lo.ToPtr("ARM64")},
	}
}

func BenchmarkNewInstanceType(b *testing.B) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	mts := benchMachineTypes()
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{{Boot: true, SizeGiB: 200, Category: "hyperdisk-balanced"}},
		},
	}
	offerings := testOfferings()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, mt := range mts {
			if it := NewInstanceType(ctx, mt, nodeClass, "us-central1", offerings); it == nil {
				b.Fatal("nil instance type")
			}
		}
	}
}

func BenchmarkNewStaticInstanceType(b *testing.B) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	mts := benchMachineTypes()
	nodeClass := &v1alpha1.GCENodeClass{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, mt := range mts {
			_ = NewStaticInstanceType(ctx, mt, nodeClass)
		}
	}
}
