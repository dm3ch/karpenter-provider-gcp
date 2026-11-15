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
	"fmt"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
)

var benchmarkInstanceTypes []*cloudprovider.InstanceType

func BenchmarkInstanceTypeProviderList(b *testing.B) {
	for _, fixture := range []struct {
		cpus     []int32
		variants int
	}{
		{cpus: []int32{4}, variants: 9},
		{cpus: []int32{2, 4, 8, 16, 32}, variants: 42},
	} {
		for _, cold := range []bool{false, true} {
			mode := "Warm"
			if cold {
				mode = "ColdIncludingReset"
			}
			b.Run(fmt.Sprintf("Machines%d/%s", len(fixture.cpus)*3, mode), func(b *testing.B) {
				ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
				p := benchmarkCatalogProvider(fixture.cpus)
				nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
					Disks: []v1alpha1.Disk{{Boot: true, SizeGiB: 100}},
				}}
				result, err := p.List(ctx, nodeClass)
				if err != nil {
					b.Fatal(err)
				}
				validateBenchmarkCatalog(b, result, len(fixture.cpus)*3, fixture.variants)
				variants := fixture.variants
				b.ReportAllocs()
				for b.Loop() {
					// Include reset in both native and CodSpeed measurements rather than
					// relying on runner-specific StopTimer/StartTimer behavior.
					if cold {
						p.staticInstanceTypesCache.Flush()
					}
					result, err = p.List(ctx, nodeClass)
					if err != nil {
						b.Fatal(err)
					}
				}
				if len(result) != variants {
					b.Fatalf("catalog changed from %d to %d variants", variants, len(result))
				}
				b.ReportMetric(float64(variants), "variants/op")
				benchmarkInstanceTypes = result
			})
		}
	}
}

func validateBenchmarkCatalog(b *testing.B, result []*cloudprovider.InstanceType, machineCount, variantCount int) {
	b.Helper()
	if len(result) != variantCount {
		b.Fatalf("got %d variants, want %d", len(result), variantCount)
	}
	names := sets.New[string]()
	for _, it := range result {
		names.Insert(it.Name)
		if len(it.Offerings) != 2 || it.Capacity.Cpu().Sign() <= 0 {
			b.Fatalf("invalid capacity or offerings for %s", it.Name)
		}
	}
	if names.Len() != machineCount {
		b.Fatalf("got %d machine names, want %d", names.Len(), machineCount)
	}
}

func benchmarkCatalogProvider(cpus []int32) *DefaultProvider {
	p := &DefaultProvider{
		authOptions:              &auth.Credential{Region: "us-central1"},
		pricingProvider:          &fakePricingProvider{},
		gkeProvider:              &fakeGKEProvider{},
		instanceTypesByName:      map[string]*computepb.MachineType{},
		instanceTypesOfferings:   map[string]sets.Set[string]{},
		staticInstanceTypesCache: cache.New(cache.NoExpiration, 0),
		unavailableOfferings:     unavailableofferings.NewUnavailableOfferingsWithCache(cache.New(cache.NoExpiration, 0)),
	}
	for _, family := range []string{"e2", "n2", "t2a"} {
		for _, cpu := range cpus {
			name := fmt.Sprintf("%s-standard-%d", family, cpu)
			mt := &computepb.MachineType{
				Name: lo.ToPtr(name), GuestCpus: lo.ToPtr(cpu), MemoryMb: lo.ToPtr(cpu * 4096),
			}
			if family == "t2a" {
				mt.Architecture = lo.ToPtr("ARM64")
			}
			p.instanceTypesByName[name] = mt
			p.instanceTypesOfferings[name] = sets.New("us-central1-a")
		}
	}
	return p
}
