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
	"context"
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing/instanceprice"
)

// knownExtras lists machine types whose prices we compute correctly but that
// neither reference source (Cyclenerd independent, GCP web) includes. Each
// entry here must have a manual validation note in the README. New EXTRA
// entries not in this set cause a non-zero exit code so they get investigated.
var knownExtras = map[string]bool{
	"a3-edgegpu-8g":         true,
	"a3-edgegpu-8g-nolssd":  true,
	"a3-megagpu-8g":         true,
	"a3-ultragpu-8g-nolssd": true,
	"g4-standard-6":         true,
	"g4-standard-12":        true,
	"g4-standard-24":        true,
}

func main() {
	filterRegion := flag.String("region", "all", `Region to compare, e.g. "us-central1", or "all"`)
	tolerance := flag.Float64("tolerance", 0.01, "Max allowed fractional price difference (default 1%)")
	noCache := flag.Bool("no-cache", false, "Ignore all caches and fetch everything fresh")
	workDir := flag.String("work-dir", "./data", "Directory for cache and output files")
	cacheTTL := flag.Duration("cache-ttl", 6*time.Hour, "Max age of reference price caches before re-fetching")
	workers := flag.Int("workers", 16, "Number of parallel workers for fetching computed prices per region")
	flag.Parse()

	if err := os.MkdirAll(*workDir, 0755); err != nil {
		log.Fatalf("creating work directory %s: %v", *workDir, err)
	}

	ctx := context.Background()

	fmt.Println("Phase 1: Reference prices")
	var (
		cyclenerdPrices RegionPrices
		gcpWebPrices    RegionPrices
		cycErr, gcpErr  error
		wg              sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); cyclenerdPrices, cycErr = getCyclenerdPrices(ctx, *workDir, *noCache, *cacheTTL) }()
	go func() { defer wg.Done(); gcpWebPrices, gcpErr = getGCPWebPrices(ctx, *workDir, *noCache, *cacheTTL) }()
	wg.Wait()
	if cycErr != nil {
		log.Fatalf("  cyclenerd prices: %v", cycErr)
	}
	if gcpErr != nil {
		log.Fatalf("  gcp web prices: %v", gcpErr)
	}

	fmt.Println("Phase 2: Computed prices (instanceprice)")
	computedPrices, regions, err := fetchInstancePrices(ctx, cyclenerdPrices, gcpWebPrices, *filterRegion, *workers)
	if err != nil {
		log.Fatalf("  computed prices: %v", err)
	}
	if len(computedPrices) == 0 {
		log.Fatal("  computed prices: no regions fetched successfully")
	}
	computedPath := filepath.Join(*workDir, "computed.json")
	if err := writePriceFile(computedPath, computedPrices); err != nil {
		log.Fatalf("  writing %s: %v", computedPath, err)
	}

	fmt.Println("Phase 3: Comparing prices")
	// refs ordered by trust: gcpweb is the authoritative source (official Google
	// billing pages); cyclenerd is the independent community cross-check.
	refs := []RegionPrices{gcpWebPrices, cyclenerdPrices}
	os.Exit(comparePrices(computedPrices, refs, []string{"gcp_web", "cyclenerd"}, nil, regions, func(m string) bool { return knownExtras[m] }, *tolerance))
}

// fetchInstancePrices fetches prices from instanceprice.Client for the
// requested regions in parallel and returns the results as RegionPrices.
// Regions are derived from the union of reference source regions when
// filterRegion is "all".
func fetchInstancePrices(ctx context.Context, cyclenerdPrices, gcpWebPrices RegionPrices, filterRegion string, workers int) (RegionPrices, []string, error) {
	var regions []string
	if filterRegion == "all" {
		regionSet := map[string]struct{}{}
		for r := range cyclenerdPrices {
			regionSet[r] = struct{}{}
		}
		for r := range gcpWebPrices {
			regionSet[r] = struct{}{}
		}
		regions = slices.Sorted(maps.Keys(regionSet))
	} else {
		regions = []string{filterRegion}
	}

	client, err := instanceprice.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("creating instanceprice client: %w", err)
	}
	defer client.Close()

	type result struct {
		region string
		prices MachinePrices
		err    error
	}

	jobs := make(chan string, len(regions))
	results := make(chan result, len(regions))
	var wg sync.WaitGroup

	for range min(workers, len(regions)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for region := range jobs {
				mps, err := client.FetchPrices(ctx, region)
				if err != nil {
					results <- result{region: region, err: err}
					continue
				}
				mp := make(MachinePrices, len(mps))
				for _, p := range mps {
					mp[p.Name] = price{OnDemand: p.OnDemandPerHour, Spot: p.SpotPerHour}
				}
				results <- result{region: region, prices: mp}
			}
		}()
	}

	for _, r := range regions {
		jobs <- r
	}
	close(jobs)
	go func() {
		wg.Wait()
		close(results)
	}()

	computed := make(RegionPrices, len(regions))
	for res := range results {
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR fetching %s: %v\n", res.region, res.err)
			continue
		}
		computed[res.region] = res.prices
	}
	fmt.Printf("  Computed prices for %d regions\n", len(computed))
	return computed, regions, nil
}
