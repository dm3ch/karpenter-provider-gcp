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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsDensityAwareReservationRelease(t *testing.T) {
	tests := []struct {
		release string
		want    bool
	}{
		{"1.37.2-gke.1325000", true},
		{"v1.37.2-gke.1325000", true},
		{"1.37.2-gke.1324999", false},
		{"1.37.2-gke.99", false},
		{"1.37.3-gke.100", true},
		{"1.37.10-gke.1", true},
		{"1.37.1-gke.9999999", false},
		{"1.38.0-gke.1", true},
		{"1.36.9-gke.9999999", false},
		{"2.0.0-gke.1", true},
		// Not placeable against a GKE build: the earlier formula keeps applying.
		{"", false},
		{"1.37.2", false},
		{"1.37-gke.1325000", false},
		{"1.37.2.1-gke.1325000", false},
		{"1.37.x-gke.1325000", false},
		{"1.37.2-gke.", false},
		{"1.37.2-gke.build", false},
	}
	for _, tt := range tests {
		t.Run(tt.release, func(t *testing.T) {
			assert.Equal(t, tt.want, IsDensityAwareReservationRelease(tt.release))
		})
	}
}

func TestResolveDensityAwareReservedMemoryMiB(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		memoryMiB    int64
		maxPods      int64
		baseMiB      int64
		want         int64
	}{
		// 16 GiB: earlier tiers give 1024+819+819 = 2662 MiB.
		{"density term is lower on COS", "n2-standard-4", 16384, 110, COSBaseOverheadMiB, 15*110 + 500},
		{"Ubuntu base overhead is larger", "n2-standard-4", 16384, 110, UbuntuBaseOverheadMiB, 15*110 + 700},
		{"low pod density lowers the reservation further", "n2-standard-4", 16384, 8, COSBaseOverheadMiB, 15*8 + 500},
		// 4 GiB: the earlier tier gives 1024 MiB, below 2150 MiB.
		{"earlier reservation is lower on small machines", "n2-standard-2", 4096, 110, COSBaseOverheadMiB, 1024},
		{"small machines keep the minimum reservation", "e2-micro", 1024, 110, COSBaseOverheadMiB, 255},
		// Ubuntu on 8 GiB: earlier tiers give 1024+819 = 1843 MiB, below 2350 MiB.
		{"earlier reservation wins on Ubuntu too", "n2-standard-2", 8192, 110, UbuntuBaseOverheadMiB, 1843},
		{"large machines are bounded by pod density", "n2-standard-64", 262144, 110, COSBaseOverheadMiB, 15*110 + 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ResolveDensityAwareReservedMemoryMiB(tt.instanceType, tt.memoryMiB, tt.maxPods, tt.baseMiB))
		})
	}
}

func TestResolveDensityAwareReservedCPUMCore(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		cpuMCore     int64
		want         int64
	}{
		{"unchanged below the cap", "n2-standard-96", 96000, 60 + 10 + 10 + 92000/400},
		// 60 + 10 + 10 + 412000*0.25% = 1110 mCPU earlier.
		{"capped at one vCPU", "m3-ultramem-416", 416000, 1000},
		{"shared-core machines keep their fixed reservation", "e2-medium", 2000, 1060},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ResolveDensityAwareReservedCPUMCore(tt.instanceType, tt.cpuMCore))
		})
	}
}

func TestResolveDensityAwareReservedResourceKeepsStorageReservations(t *testing.T) {
	cpu, memory, evict, ephEvict, ephSys := ResolveDensityAwareReservedResource("n2-standard-4", 4000, 16384, 110, COSBaseOverheadMiB, 100, 0, 0)

	assert.Equal(t, int64(80), cpu)
	assert.Equal(t, int64(2150), memory)
	assert.Equal(t, int64(100), evict)
	assert.Equal(t, int64(10), ephEvict)
	assert.Equal(t, int64(41), ephSys) // min(50, 35+6, 100)
}
