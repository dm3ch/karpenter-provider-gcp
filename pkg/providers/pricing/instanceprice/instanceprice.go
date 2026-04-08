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

// Package instanceprice computes hourly on-demand and spot prices for GCP
// machine types. The current implementation fetches prices from the Cyclenerd
// community CSV (gcloud-compute.com). A subsequent release will replace this
// with an implementation based on the GCP Cloud Billing Catalog API.
package instanceprice

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const cyclenerdURL = "https://gcloud-compute.com/machine-types-regions.csv"

// csvHTTPClient has an explicit timeout so a slow or hung Cyclenerd server
// cannot block the pricing controller indefinitely.
var csvHTTPClient = &http.Client{Timeout: 30 * time.Second}

// MachinePrice holds the hourly price for a single machine type.
// SpotPerHour is zero when spot capacity is not available for that machine.
type MachinePrice struct {
	Name            string
	OnDemandPerHour float64
	SpotPerHour     float64
}

// Client fetches and caches GCP instance prices. Safe for concurrent use.
// Create a new Client to force a price refresh.
type Client struct {
	mu     sync.RWMutex
	cached map[string][]MachinePrice // region → prices; nil until first fetch
}

// New creates a Client. The constructor is intentionally lightweight; no
// network calls are made until the first FetchPrices call.
func New(_ context.Context) (*Client, error) {
	return &Client{}, nil
}

// Close is a no-op for the Cyclenerd-based implementation.
func (c *Client) Close() {}

// FetchPrices returns hourly prices for all machine types available in region.
// The underlying CSV is fetched once and cached for the lifetime of the Client.
// The HTTP fetch happens outside any lock so that concurrent callers do not
// block each other waiting for the network; only the cache-store is guarded.
func (c *Client) FetchPrices(ctx context.Context, region string) ([]MachinePrice, error) {
	c.mu.RLock()
	cached := c.cached
	c.mu.RUnlock()
	if cached != nil {
		return cached[region], nil
	}

	all, err := fetchAll(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.cached == nil {
		c.cached = all
	}
	prices := c.cached[region]
	c.mu.Unlock()
	return prices, nil
}

// IsBlacklisted reports whether name is an intentionally unpriced machine type.
// The Cyclenerd-based implementation has no blacklisted machines; this list
// will be populated when the SKU-based implementation lands.
func IsBlacklisted(_ string) bool {
	return false
}

// fetchAll downloads the Cyclenerd CSV and returns prices indexed by region.
func fetchAll(ctx context.Context) (map[string][]MachinePrice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cyclenerdURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := csvHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching Cyclenerd CSV: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cyclenerd CSV: HTTP %d", resp.StatusCode)
	}
	return parseCSV(resp.Body)
}

func parseCSV(r io.Reader) (map[string][]MachinePrice, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}

	nameCol, regionCol, onDemandCol, spotCol, err := findColumns(header)
	if err != nil {
		return nil, err
	}

	return readRows(cr, nameCol, regionCol, onDemandCol, spotCol)
}

func findColumns(header []string) (nameCol, regionCol, onDemandCol, spotCol int, err error) {
	nameCol, regionCol, onDemandCol, spotCol = -1, -1, -1, -1
	for i, h := range header {
		switch strings.ToLower(h) {
		case "name":
			nameCol = i
		case "region":
			regionCol = i
		case "hour":
			onDemandCol = i
		case "hourspot":
			spotCol = i
		}
	}
	if nameCol < 0 || regionCol < 0 || onDemandCol < 0 || spotCol < 0 {
		return -1, -1, -1, -1, fmt.Errorf("CSV missing required columns (name/region/hour/hourSpot); got: %v", header)
	}
	return nameCol, regionCol, onDemandCol, spotCol, nil
}

func readRows(cr *csv.Reader, nameCol, regionCol, onDemandCol, spotCol int) (map[string][]MachinePrice, error) {
	result := make(map[string][]MachinePrice)
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading CSV row: %w", err)
		}
		if len(rec) <= max(nameCol, regionCol, onDemandCol, spotCol) {
			continue
		}
		onDemand, err := strconv.ParseFloat(rec[onDemandCol], 64)
		if err != nil || onDemand == 0 {
			continue
		}
		mp := MachinePrice{
			Name:            rec[nameCol],
			OnDemandPerHour: onDemand,
		}
		if spot, err := strconv.ParseFloat(rec[spotCol], 64); err == nil && spot > 0 {
			mp.SpotPerHour = spot
		}
		result[rec[regionCol]] = append(result[rec[regionCol]], mp)
	}
	return result, nil
}
