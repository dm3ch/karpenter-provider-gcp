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
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// FindingKind classifies a price comparison result.
type FindingKind string

const (
	FindingMismatch  FindingKind = "MISMATCH"
	FindingMissing   FindingKind = "MISSING"
	FindingExtra     FindingKind = "EXTRA"
	FindingExtraNew  FindingKind = "EXTRA_NEW"
	FindingUnavail   FindingKind = "UNAVAIL"
	FindingBlacklist FindingKind = "BLACKLIST"
)

// priceKind identifies which price tier a finding refers to.
type priceKind string

const (
	priceKindOnDemand priceKind = "OnDemand"
	priceKindSpot     priceKind = "Spot"
)

type finding struct {
	kind        FindingKind
	priceType   priceKind
	region      string
	machineType string
	computed    float64
	refs        []float64 // one entry per reference source, 0 = source has no price
}

// comparePrices diffs computed prices against ordered reference sources and
// prints a report. refs is ordered by trust: refs[0] is the most authoritative.
// refLabels names each source for output; nil falls back to "ref0", "ref1", …
// availability maps region → machine type → true for every machine returned by
// the Compute Engine API; nil disables the UNAVAIL classification.
// isKnownExtra returns true for machine types that are intentionally absent from
// all reference sources (e.g. newly launched hardware not yet listed).
// Returns 1 if any actionable findings (MISMATCH/MISSING/EXTRA_NEW) are present.
func comparePrices(
	computedPrices RegionPrices,
	refs []RegionPrices,
	refLabels []string,
	availability RegionAvailability,
	regions []string,
	isKnownExtra func(string) bool,
	tolerance float64,
) int {
	var findings []finding
	var checked int

	for _, region := range regions {
		computedMachines := computedPrices[region]

		// Build union of all machine names present in any reference source.
		allRefNames := map[string]struct{}{}
		for _, ref := range refs {
			for name := range ref[region] {
				allRefNames[name] = struct{}{}
			}
		}

		for _, machine := range slices.Sorted(maps.Keys(allRefNames)) {
			if isNonMachineTypeEntry(machine) {
				continue
			}

			refPrices := make([]float64, len(refs))
			for i, ref := range refs {
				refPrices[i] = ref[region][machine].OnDemand
			}

			// Skip if no reference has an on-demand price.
			if !slices.ContainsFunc(refPrices, func(p float64) bool { return p > 0 }) {
				continue
			}
			// Treat a nil computed region (fetch error) the same as per-machine MISSING.
			if computedMachines == nil {
				findings = append(findings, finding{
					kind:        classifyMissing(machine, region, availability),
					priceType:   priceKindOnDemand,
					machineType: machine,
					region:      region,
					refs:        refPrices,
				})
				continue
			}

			computed, ok := computedMachines[machine]
			if !ok {
				findings = append(findings, finding{
					kind:        classifyMissing(machine, region, availability),
					priceType:   priceKindOnDemand,
					machineType: machine,
					region:      region,
					refs:        refPrices,
				})
				continue
			}
			checked++

			if classifyMismatch(computed.OnDemand, refPrices, tolerance) {
				findings = append(findings, finding{
					kind:        FindingMismatch,
					priceType:   priceKindOnDemand,
					machineType: machine,
					region:      region,
					computed:    computed.OnDemand,
					refs:        refPrices,
				})
			}

			spotRefs := make([]float64, len(refs))
			hasSpotRef := false
			for i, ref := range refs {
				spotRefs[i] = ref[region][machine].Spot
				if spotRefs[i] > 0 {
					hasSpotRef = true
				}
			}
			if hasSpotRef && classifyMismatch(computed.Spot, spotRefs, tolerance) {
				findings = append(findings, finding{
					kind: FindingMismatch, priceType: priceKindSpot, machineType: machine, region: region,
					computed: computed.Spot, refs: spotRefs,
				})
			}
		}

		for machine, p := range computedMachines {
			inAnyRef := false
			for _, ref := range refs {
				if _, ok := ref[region][machine]; ok {
					inAnyRef = true
					break
				}
			}
			if !inAnyRef {
				kind := FindingExtraNew
				if isKnownExtra(machine) {
					kind = FindingExtra
				}
				findings = append(findings, finding{
					kind: kind, priceType: priceKindOnDemand, machineType: machine, region: region,
					computed: p.OnDemand,
				})
			}
		}
	}

	slices.SortFunc(findings, func(a, b finding) int {
		if a.region != b.region {
			return strings.Compare(a.region, b.region)
		}
		if a.machineType != b.machineType {
			return strings.Compare(a.machineType, b.machineType)
		}
		return strings.Compare(string(a.priceType), string(b.priceType))
	})

	labelFor := func(i int) string {
		if i < len(refLabels) {
			return refLabels[i]
		}
		return fmt.Sprintf("ref%d", i)
	}

	var mismatches, missing, extra, extraNew, unavail, blacklisted int
	for _, f := range findings {
		switch f.kind {
		case FindingUnavail:
			unavail++
			continue
		case FindingBlacklist:
			blacklisted++
			continue
		case FindingExtra:
			extra++
			continue
		}
		computed := "n/a"
		if f.computed != 0 {
			computed = fmt.Sprintf("%.6f", f.computed)
		}
		refParts := make([]string, len(f.refs))
		for i, r := range f.refs {
			refParts[i] = fmt.Sprintf("%s=%s", labelFor(i), fmtRef(f.computed, r, tolerance))
		}
		fmt.Printf("%-9s %-40s %-28s %-8s computed=%-12s %s\n",
			f.kind, f.machineType, f.region, f.priceType,
			computed, strings.Join(refParts, "  "))
		switch f.kind {
		case FindingMismatch:
			mismatches++
		case FindingMissing:
			missing++
		case FindingExtraNew:
			extraNew++
		}
	}

	fmt.Printf("\nSummary over %d region(s): checked=%d  mismatches=%d  missing=%d  extra=%d  extra_new=%d  unavail=%d  blacklisted=%d  tolerance=%.0f%%\n",
		len(regions), checked, mismatches, missing, extra, extraNew, unavail, blacklisted, tolerance*100)

	if mismatches > 0 || missing > 0 || extraNew > 0 {
		return 1
	}
	return 0
}

// classifyMissing returns the appropriate FindingKind for a machine that is in
// reference sources but absent from computed prices.
func classifyMissing(machine, region string, availability RegionAvailability) FindingKind {
	if av, ok := availability[region]; ok && !av[machine] {
		return FindingUnavail
	}
	return FindingMissing
}

// classifyMismatch returns true when the computed price genuinely disagrees with
// the reference sources. refs is ordered by trust: the first source that has a
// price (non-zero) is authoritative. Its verdict is final — lower-trust sources
// cannot override it in either direction.
//
//   - First ref with a price agrees  → no mismatch (even if lower refs disagree).
//   - First ref with a price disagrees → mismatch (even if lower refs agree).
//   - No ref has a price → no mismatch.
func classifyMismatch(computed float64, refs []float64, tolerance float64) bool {
	for _, ref := range refs {
		if ref == 0 {
			continue
		}
		return differs(computed, ref, tolerance)
	}
	return false
}

func differs(computed, ref, tolerance float64) bool {
	return ref > 0 && fracDiff(computed, ref) > tolerance
}

func fracDiff(a, b float64) float64 {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return 1
	}
	return math.Abs(a-b) / b
}

// isNonMachineTypeEntry returns true for reference entries that are not standard
// Compute Engine machine types and should be excluded from comparison:
//   - Sole-tenant node types (e.g. "c4-node-192-384") — not standard VMs
//   - Standalone GPU add-on prices (e.g. "NVIDIA T4") — per-GPU prices, not machine types
//   - Custom machine type per-unit prices (e.g. "Custom vCPUs") — per-unit prices
//   - Platform-specific variants (e.g. "n1-highcpu-96 Skylake Platform only") — not in Compute API
func isNonMachineTypeEntry(name string) bool {
	return strings.Contains(name, "-node-") ||
		strings.HasPrefix(name, "NVIDIA ") ||
		strings.HasPrefix(name, "Custom ") ||
		strings.Contains(name, " Skylake") ||
		strings.Contains(name, " Platform")
}

// fmtRef formats a reference price for the output line:
//   - "n/a"              — source has no price for this machine
//   - "0.388000"         — source has a price but computed is absent (MISSING)
//   - "0.388000(ok)"     — within tolerance
//   - "0.400000(+3.1%)"  — mismatch, showing deviation from reference
func fmtRef(computed, ref, tolerance float64) string {
	if ref == 0 {
		return "n/a"
	}
	if computed == 0 {
		return fmt.Sprintf("%.6f", ref)
	}
	deviation := fracDiff(computed, ref)
	if deviation <= tolerance {
		return fmt.Sprintf("%.6f(ok)", ref)
	}
	percent := deviation * 100
	if computed < ref {
		percent = -percent
	}
	return fmt.Sprintf("%.6f(%+.1f%%)", ref, percent)
}
