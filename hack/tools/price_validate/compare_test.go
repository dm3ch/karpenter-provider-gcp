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

package main

import (
	"testing"
)

const tol = 0.01 // 1% tolerance used throughout

func TestClassifyMismatch(t *testing.T) {
	tests := []struct {
		name     string
		computed float64
		refs     []float64
		want     bool
	}{
		{
			name:     "all refs agree with computed",
			computed: 1.0, refs: []float64{1.0, 1.0}, want: false,
		},
		{
			name:     "no refs have prices",
			computed: 1.0, refs: []float64{0, 0}, want: false,
		},
		{
			name:     "both refs disagree",
			computed: 2.0, refs: []float64{1.0, 1.0}, want: true,
		},
		{
			name:     "first ref agrees — clears second disagreement",
			computed: 1.0, refs: []float64{1.0, 2.0}, want: false,
		},
		{
			name:     "first ref disagrees — second ref agrees — still mismatch (lower trust cannot clear higher)",
			computed: 2.0, refs: []float64{1.0, 2.0}, want: true,
		},
		{
			name:     "only first ref has price and agrees",
			computed: 1.0, refs: []float64{1.0, 0}, want: false,
		},
		{
			name:     "only first ref has price and disagrees",
			computed: 2.0, refs: []float64{1.0, 0}, want: true,
		},
		{
			name:     "only second ref has price and disagrees",
			computed: 2.0, refs: []float64{0, 1.0}, want: true,
		},
		{
			name:     "within tolerance — not a mismatch",
			computed: 1.005, refs: []float64{1.0, 1.0}, want: false,
		},
		{
			name:     "just over tolerance — mismatch",
			computed: 1.02, refs: []float64{1.0, 1.0}, want: true,
		},
		{
			name:     "single ref, no price",
			computed: 1.0, refs: []float64{0}, want: false,
		},
		{
			// computed=0 means we produce no price for a machine the ref lists;
			// fracDiff(0, 1.0)=1.0 >> tolerance so this must be a mismatch.
			name:     "computed absent (0), ref present",
			computed: 0, refs: []float64{1.0}, want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMismatch(tt.computed, tt.refs, tol)
			if got != tt.want {
				t.Errorf("classifyMismatch(%v, %v, %v) = %v; want %v",
					tt.computed, tt.refs, tol, got, tt.want)
			}
		})
	}
}

func TestClassifyMissing(t *testing.T) {
	inAvail := RegionAvailability{"us-central1": {"e2-standard-2": true}}
	notAvail := RegionAvailability{"us-central1": {"e2-standard-2": false}}

	tests := []struct {
		name         string
		machine      string
		region       string
		availability RegionAvailability
		want         FindingKind
	}{
		{
			name: "available, not blacklisted → MISSING",
			machine: "e2-standard-2", region: "us-central1",
			availability: inAvail, want: FindingMissing,
		},
		{
			name: "machine explicitly false in availability → UNAVAIL",
			machine: "e2-standard-2", region: "us-central1",
			availability: notAvail, want: FindingUnavail,
		},
		{
			// Region absent from map: we don't know → treat as available → MISSING.
			name: "region absent from availability → MISSING (not UNAVAIL)",
			machine: "e2-standard-2", region: "us-east1",
			availability: inAvail, want: FindingMissing,
		},
		{
			name: "nil availability → MISSING",
			machine: "e2-standard-2", region: "us-central1",
			availability: nil, want: FindingMissing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMissing(tt.machine, tt.region, tt.availability)
			if got != tt.want {
				t.Errorf("classifyMissing(%q, %q, ...) = %v; want %v",
					tt.machine, tt.region, got, tt.want)
			}
		})
	}
}

func TestComparePrices_Findings(t *testing.T) {
	noExtra := func(string) bool { return false }
	knownExtra := func(m string) bool { return m == "known-extra-machine" }

	computed := RegionPrices{
		"us-central1": {
			"e2-standard-2":      {OnDemand: 1.0},
			"extra-machine":      {OnDemand: 5.0}, // in computed, not in refs
			"known-extra-machine": {OnDemand: 5.0}, // in computed, not in refs, but known
		},
	}
	refs := []RegionPrices{
		// ref0 (higher trust)
		{
			"us-central1": {
				"e2-standard-2": {OnDemand: 1.0},
				"missing-machine": {OnDemand: 2.0},
			},
		},
		// ref1 (lower trust)
		{
			"us-central1": {
				"e2-standard-2": {OnDemand: 1.0},
			},
		},
	}
	regions := []string{"us-central1"}

	exit := comparePrices(computed, refs, nil, nil, regions, knownExtra, tol)
	if exit != 1 {
		t.Errorf("expected exit=1 (MISSING + EXTRA_NEW); got %d", exit)
	}

	// All matching, no findings — should return 0.
	exactRefs := []RegionPrices{{"us-central1": {"e2-standard-2": {OnDemand: 1.0}}}}
	perfect := RegionPrices{"us-central1": {"e2-standard-2": {OnDemand: 1.0}}}
	exit2 := comparePrices(perfect, exactRefs, nil, nil, regions, noExtra, tol)
	if exit2 != 0 {
		t.Errorf("expected exit=0 for matching prices; got %d", exit2)
	}
}

func TestComparePrices_ExtraKinds(t *testing.T) {
	knownExtra := func(m string) bool { return m == "known" }

	computed := RegionPrices{
		"us-central1": {
			"known":   {OnDemand: 1.0},
			"unknown": {OnDemand: 2.0},
		},
	}
	refs := []RegionPrices{{}} // no ref has any machines
	regions := []string{"us-central1"}

	exit := comparePrices(computed, refs, nil, nil, regions, knownExtra, tol)
	// "unknown" is EXTRA_NEW → exit 1; "known" is EXTRA → silent
	if exit != 1 {
		t.Errorf("expected exit=1 for EXTRA_NEW; got %d", exit)
	}

	// Both known → exit 0
	computed2 := RegionPrices{"us-central1": {"known": {OnDemand: 1.0}}}
	exit2 := comparePrices(computed2, refs, nil, nil, regions, knownExtra, tol)
	if exit2 != 0 {
		t.Errorf("expected exit=0 when all extras are known; got %d", exit2)
	}
}

func TestComparePrices_SpotMismatch(t *testing.T) {
	noExtra := func(string) bool { return false }
	regions := []string{"us-central1"}

	// Spot price in ref differs from computed by more than tolerance.
	computed := RegionPrices{
		"us-central1": {"e2-standard-2": {OnDemand: 1.0, Spot: 0.2}},
	}
	refs := []RegionPrices{
		{"us-central1": {"e2-standard-2": {OnDemand: 1.0, Spot: 0.3}}},
	}
	exit := comparePrices(computed, refs, nil, nil, regions, noExtra, tol)
	if exit != 1 {
		t.Errorf("expected exit=1 for spot mismatch; got %d", exit)
	}

	// No spot price in any ref — spot mismatch must be ignored.
	refsNoSpot := []RegionPrices{
		{"us-central1": {"e2-standard-2": {OnDemand: 1.0}}},
	}
	exit2 := comparePrices(computed, refsNoSpot, nil, nil, regions, noExtra, tol)
	if exit2 != 0 {
		t.Errorf("expected exit=0 when no ref has spot price; got %d", exit2)
	}
}

func TestIsNonMachineTypeEntry(t *testing.T) {
	yes := []string{
		"c4-node-192-384",
		"NVIDIA T4",
		"Custom vCPUs",
		"n1-highcpu-96 Skylake Platform only",
		"n1-standard-4 Platform only",
	}
	no := []string{
		"e2-standard-2",
		"n2-highmem-4",
		"a3-megagpu-8g",
		"m2-ultramem-208",
	}

	for _, name := range yes {
		if !isNonMachineTypeEntry(name) {
			t.Errorf("isNonMachineTypeEntry(%q) = false; want true", name)
		}
	}
	for _, name := range no {
		if isNonMachineTypeEntry(name) {
			t.Errorf("isNonMachineTypeEntry(%q) = true; want false", name)
		}
	}
}

func TestFmtRef(t *testing.T) {
	tests := []struct {
		computed, ref float64
		want          string
	}{
		{computed: 1.0, ref: 0, want: "n/a"},
		{computed: 0, ref: 1.0, want: "1.000000"},
		{computed: 1.0, ref: 1.0, want: "1.000000(ok)"},
		{computed: 1.02, ref: 1.0, want: "1.000000(+2.0%)"},
		{computed: 0.98, ref: 1.0, want: "1.000000(-2.0%)"},
	}

	for _, tt := range tests {
		got := fmtRef(tt.computed, tt.ref, tol)
		if got != tt.want {
			t.Errorf("fmtRef(%v, %v, %v) = %q; want %q", tt.computed, tt.ref, tol, got, tt.want)
		}
	}
}
