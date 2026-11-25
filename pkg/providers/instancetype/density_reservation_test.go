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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/operator/options"
)

const (
	preDensityRelease = "1.37.2-gke.1324999"
	densityRelease    = "1.37.2-gke.1325000"
)

// newReservationTestProvider offers one amd64 and one arm64 machine type with
// 16 GiB each, whose earlier tiered memory reservation is 2662 MiB.
func newReservationTestProvider() *DefaultProvider {
	p := newTestProvider()
	p.instanceTypesByName["t2a-standard-4"] = &computepb.MachineType{
		Name:         lo.ToPtr("t2a-standard-4"),
		GuestCpus:    lo.ToPtr[int32](4),
		MemoryMb:     lo.ToPtr[int32](16384),
		Architecture: lo.ToPtr("ARM64"),
	}
	p.instanceTypesOfferings["t2a-standard-4"] = sets.New("us-central1-a")
	return p
}

func reservationNodeClass(family string, amd64Release, arm64Release string) *v1alpha1.GCENodeClass {
	image := func(arch, release string) v1alpha1.Image {
		return v1alpha1.Image{
			SourceImage:       "projects/p/global/images/" + arch,
			KubernetesVersion: release,
			Requirements: []corev1.NodeSelectorRequirement{
				{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{arch}},
			},
		}
	}
	nodeClass := &v1alpha1.GCENodeClass{
		Status: v1alpha1.GCENodeClassStatus{Images: []v1alpha1.Image{
			image("amd64", amd64Release),
			image("arm64", arm64Release),
		}},
	}
	if family != "" {
		nodeClass.Spec.ImageFamily = lo.ToPtr(family)
	}
	return nodeClass
}

func reservedByType(t *testing.T, p *DefaultProvider, nodeClass *v1alpha1.GCENodeClass) map[string]corev1.ResourceList {
	t.Helper()
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	instanceTypes, err := p.List(ctx, nodeClass)
	require.NoError(t, err)
	return lo.SliceToMap(instanceTypes, func(it *cloudprovider.InstanceType) (string, corev1.ResourceList) {
		return it.Name, it.Overhead.KubeReserved
	})
}

func memoryMiB(reserved corev1.ResourceList) int64 {
	return reserved.Memory().Value() / (1024 * 1024)
}

func TestListReservationFollowsSelectedImageRelease(t *testing.T) {
	tests := []struct {
		name         string
		family       string
		amd64Release string
		arm64Release string
		wantAMD64    int64
		wantARM64    int64
	}{
		{"COS on a release with the density-aware formula", v1alpha1.ImageFamilyContainerOptimizedOS, densityRelease, densityRelease, 15*110 + 500, 15*110 + 500},
		{"Ubuntu has a larger base overhead", v1alpha1.ImageFamilyUbuntu, densityRelease, densityRelease, 15*110 + 700, 15*110 + 700},
		{"release just before the threshold keeps the earlier formula", v1alpha1.ImageFamilyContainerOptimizedOS, preDensityRelease, preDensityRelease, 2662, 2662},
		{"each architecture follows its own image", v1alpha1.ImageFamilyContainerOptimizedOS, densityRelease, preDensityRelease, 15*110 + 500, 2662},
		{"custom images keep the earlier formula", "", densityRelease, densityRelease, 2662, 2662},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reserved := reservedByType(t, newReservationTestProvider(), reservationNodeClass(tt.family, tt.amd64Release, tt.arm64Release))

			require.Equal(t, tt.wantAMD64, memoryMiB(reserved["n2-standard-4"]))
			require.Equal(t, tt.wantARM64, memoryMiB(reserved["t2a-standard-4"]))
		})
	}
}

// A refreshed image release or a changed image family must not be served from
// instance types cached for the previous one.
func TestListReservationIsNotServedFromStaleCache(t *testing.T) {
	p := newReservationTestProvider()
	cos := v1alpha1.ImageFamilyContainerOptimizedOS

	before := reservedByType(t, p, reservationNodeClass(cos, preDensityRelease, preDensityRelease))
	after := reservedByType(t, p, reservationNodeClass(cos, densityRelease, densityRelease))
	ubuntu := reservedByType(t, p, reservationNodeClass(v1alpha1.ImageFamilyUbuntu, densityRelease, densityRelease))
	rolledBack := reservedByType(t, p, reservationNodeClass(cos, preDensityRelease, preDensityRelease))

	require.Equal(t, int64(2662), memoryMiB(before["n2-standard-4"]))
	require.Equal(t, int64(2150), memoryMiB(after["n2-standard-4"]))
	require.Equal(t, int64(2350), memoryMiB(ubuntu["n2-standard-4"]))
	require.Equal(t, int64(2662), memoryMiB(rolledBack["n2-standard-4"]))
}

func TestListReservationUsesNodeClassMaxPodsAndOverrides(t *testing.T) {
	nodeClass := reservationNodeClass(v1alpha1.ImageFamilyContainerOptimizedOS, densityRelease, densityRelease)
	nodeClass.Spec.KubeletConfiguration = &v1alpha1.KubeletConfiguration{MaxPods: lo.ToPtr[int32](32)}

	reserved := reservedByType(t, newReservationTestProvider(), nodeClass)
	require.Equal(t, int64(15*32+500), memoryMiB(reserved["n2-standard-4"]))

	nodeClass.Spec.KubeletConfiguration.KubeReserved = map[string]v1alpha1.KubeletQuantity{"memory": "1Gi"}
	reserved = reservedByType(t, newReservationTestProvider(), nodeClass)
	require.Equal(t, int64(1024), memoryMiB(reserved["n2-standard-4"]), "explicit kubeReserved still overrides the default")
}
