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

package utils

import (
	"strconv"
	"strings"
)

const (
	// DensityAwareReservationMinRelease is the first GKE release whose node
	// pools use the pod-density-aware memory reservation and capped CPU
	// reservation:
	// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/plan-node-sizes#memory_and_cpu_reservations
	DensityAwareReservationMinRelease = "1.37.2-gke.1325000"

	// COSBaseOverheadMiB and UbuntuBaseOverheadMiB are the BaseOverhead term of
	// the density-aware memory formula for each node image family.
	COSBaseOverheadMiB    int64 = 500
	UbuntuBaseOverheadMiB int64 = 700

	// perPodMemoryMiB is the memory the density-aware formula reserves per pod
	// the node can run.
	perPodMemoryMiB int64 = 15
	// maxDensityAwareReservedCPUMCore caps the total CPU reservation of
	// non-fractional machines under the density-aware formula.
	maxDensityAwareReservedCPUMCore int64 = 1000
)

// IsDensityAwareReservationRelease reports whether a GKE release such as
// "1.37.2-gke.1325000" (an optional leading "v" is ignored) is at or after
// DensityAwareReservationMinRelease. Releases that are not in GKE form, such as
// an upstream "1.37.2", cannot be placed relative to a GKE build and report
// false so that the earlier formula keeps applying.
func IsDensityAwareReservationRelease(release string) bool {
	got, ok := parseGKERelease(release)
	if !ok {
		return false
	}
	min, _ := parseGKERelease(DensityAwareReservationMinRelease)
	for i := range got {
		if got[i] != min[i] {
			return got[i] > min[i]
		}
	}
	return true
}

// parseGKERelease splits "<major>.<minor>.<patch>-gke.<build>" into its four
// numbers.
func parseGKERelease(release string) ([4]int64, bool) {
	var out [4]int64
	version, build, found := strings.Cut(strings.TrimPrefix(release, "v"), "-gke.")
	if !found {
		return out, false
	}
	fields := strings.Split(version, ".")
	if len(fields) != 3 {
		return out, false
	}
	fields = append(fields, build)
	for i, field := range fields {
		n, err := strconv.ParseInt(field, 10, 64)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ResolveDensityAwareReservedMemoryMiB returns the memory reserved for
// Kubernetes and system components on a node running a release at or after
// DensityAwareReservationMinRelease: the lower of the earlier tiered reservation
// and 15 MiB per pod plus the image family's base overhead. The 100 MiB
// eviction threshold is separate, as in the earlier formula.
func ResolveDensityAwareReservedMemoryMiB(instanceType string, memoryMiB, maxPods, baseOverheadMiB int64) int64 {
	return min(ResolveReservedMemoryMiB(instanceType, memoryMiB), perPodMemoryMiB*maxPods+baseOverheadMiB)
}

// ResolveDensityAwareReservedCPUMCore returns the CPU reserved on a node
// running a release at or after DensityAwareReservationMinRelease: the earlier
// tiered reservation capped at 1000 mCPU. Shared-core E2 machine types keep
// their fixed reservation.
func ResolveDensityAwareReservedCPUMCore(instanceType string, cpuMCore int64) int64 {
	reserved := ResolveReservedCPUMCore(instanceType, cpuMCore)
	if isSharedCoreE2(instanceType) {
		return reserved
	}
	return min(reserved, maxDensityAwareReservedCPUMCore)
}

// ResolveDensityAwareReservedResource is ResolveReservedResource for a node
// running a release at or after DensityAwareReservationMinRelease. Only the CPU
// and memory reservations differ; the eviction memory and ephemeral storage
// values are unchanged.
func ResolveDensityAwareReservedResource(instanceType string, cpuMCore, memoryMiB, maxPods, baseOverheadMiB, bootDiskGiB, totalSSDGiB, localSSDCount int64) (int64, int64, int64, int64, int64) {
	_, _, evictionMemory, ephemeralEviction, ephemeralSystem := ResolveReservedResource(instanceType, cpuMCore, memoryMiB, bootDiskGiB, totalSSDGiB, localSSDCount)
	return ResolveDensityAwareReservedCPUMCore(instanceType, cpuMCore),
		ResolveDensityAwareReservedMemoryMiB(instanceType, memoryMiB, maxPods, baseOverheadMiB),
		evictionMemory, ephemeralEviction, ephemeralSystem
}
