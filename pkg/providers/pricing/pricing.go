/*
Copyright 2025 The CloudPilot AI Authors.

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

package pricing

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing/instanceprice"
)

//go:embed initial-prices.json
var initialPricesData []byte

type Provider interface {
	LivenessProbe(*http.Request) error
	InstanceTypes() []string
	OnDemandPrice(string) (float64, bool)
	SpotPrice(string, string) (float64, bool)
	UpdatePrices(context.Context) error
}

type pricesStorage map[string]float64

// initialPricesFile matches the price_validate computed.json / update-pricing CI
// output format so that make update-pricing can copy that file directly.
type initialPricesFile struct {
	Prices map[string]map[string]initialPrice `json:"prices"`
}

type initialPrice struct {
	OnDemand float64 `json:"on_demand"`
	Spot     float64 `json:"spot"`
}

type DefaultProvider struct {
	region string

	mu             sync.RWMutex
	onDemandPrices pricesStorage
	spotPrices     pricesStorage
}

func NewDefaultProvider(ctx context.Context, region string) (*DefaultProvider, error) {
	p := &DefaultProvider{
		region:         region,
		onDemandPrices: make(pricesStorage),
		spotPrices:     make(pricesStorage),
	}
	if err := p.Reset(); err != nil {
		return nil, err
	}
	log.FromContext(ctx).Info("Loaded initial prices", "region", region, "count", len(p.onDemandPrices))
	return p, nil
}

// Reset loads on-demand prices from the embedded initial-prices.json.
// Spot prices are intentionally not loaded — the embedded file is refreshed
// weekly and spot prices change too rapidly to be useful as a fallback.
// All parsing happens outside the lock; only the final swap is guarded.
func (p *DefaultProvider) Reset() error {
	var data initialPricesFile
	if err := json.Unmarshal(initialPricesData, &data); err != nil {
		return fmt.Errorf("parsing initial-prices.json: %w", err)
	}

	regionPrices, ok := data.Prices[p.region]
	if !ok || len(regionPrices) == 0 {
		return fmt.Errorf("no initial prices found for region %s", p.region)
	}

	od := make(pricesStorage, len(regionPrices))
	for name, ip := range regionPrices {
		od[name] = ip.OnDemand
	}

	p.mu.Lock()
	p.onDemandPrices = od
	p.spotPrices = make(pricesStorage)
	p.mu.Unlock()
	return nil
}

func (p *DefaultProvider) LivenessProbe(_ *http.Request) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.onDemandPrices) == 0 {
		return fmt.Errorf("pricing provider has no on-demand prices loaded")
	}
	return nil
}

func (p *DefaultProvider) InstanceTypes() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	types := make([]string, 0, len(p.onDemandPrices))
	for t := range p.onDemandPrices {
		types = append(types, t)
	}
	return types
}

func (p *DefaultProvider) OnDemandPrice(instanceType string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	price, ok := p.onDemandPrices[instanceType]
	return price, ok
}

// SpotPrice ignores zone (GCP prices are regional).
// Falls back to 40% of on-demand when no spot price is known.
func (p *DefaultProvider) SpotPrice(instanceType string, _ string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if price, ok := p.spotPrices[instanceType]; ok {
		return price, true
	}
	if odPrice, ok := p.onDemandPrices[instanceType]; ok {
		return odPrice * 0.4, true
	}
	return 0, false
}

// UpdatePrices fetches fresh prices from the upstream source.
// A new Client is created on each call so cached data from the previous run
// is discarded and prices are always fetched fresh.
// All map building happens outside the lock; only the final swap is guarded.
func (p *DefaultProvider) UpdatePrices(ctx context.Context) error {
	client, err := instanceprice.New(ctx)
	if err != nil {
		return fmt.Errorf("creating instanceprice client: %w", err)
	}
	defer client.Close()

	prices, err := client.FetchPrices(ctx, p.region)
	if err != nil {
		return fmt.Errorf("fetching prices for %s: %w", p.region, err)
	}
	if len(prices) == 0 {
		return fmt.Errorf("no prices retrieved for region %s", p.region)
	}

	od := make(pricesStorage, len(prices))
	spot := make(pricesStorage, len(prices))
	for _, mp := range prices {
		od[mp.Name] = mp.OnDemandPerHour
		if mp.SpotPerHour > 0 {
			spot[mp.Name] = mp.SpotPerHour
		}
	}

	p.mu.Lock()
	p.onDemandPrices = od
	p.spotPrices = spot
	p.mu.Unlock()

	log.FromContext(ctx).Info("Updated prices", "region", p.region, "onDemand", len(od), "spot", len(spot))
	return nil
}
