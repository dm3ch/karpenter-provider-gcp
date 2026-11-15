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

package gke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/patrickmn/go-cache"
	containerv1 "google.golang.org/api/container/v1"
	googleapioption "google.golang.org/api/option"
)

var benchmarkServerConfig *containerv1.ServerConfig

func BenchmarkGetServerConfig(b *testing.B) {
	b.Run("Warm", func(b *testing.B) { benchmarkServerConfigLookup(b, false) })
	b.Run("RefreshIncludingReset", func(b *testing.B) { benchmarkServerConfigLookup(b, true) })
}

func benchmarkServerConfigLookup(b *testing.B, refresh bool) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"channels":[{"channel":"STABLE","defaultVersion":"1.35.1-gke.1000000"}]}`))
	}))
	b.Cleanup(srv.Close)
	ctx := context.Background()
	svc, err := containerv1.NewService(ctx,
		googleapioption.WithoutAuthentication(),
		googleapioption.WithHTTPClient(srv.Client()),
		googleapioption.WithEndpoint(srv.URL+"/"),
	)
	if err != nil {
		b.Fatal(err)
	}
	p := &DefaultProvider{
		containerService: svc, projectID: "benchmark", nodeLocation: "us-central1",
		serverConfigCache: cache.New(cache.NoExpiration, 0),
	}
	result, err := p.GetServerConfig(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if len(result.Channels) != 1 || result.Channels[0].DefaultVersion != "1.35.1-gke.1000000" || calls.Load() != 1 {
		b.Fatal("expected one initial request and a valid server configuration")
	}
	before := calls.Load()
	b.ReportAllocs()
	for b.Loop() {
		if refresh {
			p.serverConfigCache.Flush()
		}
		result, err = p.GetServerConfig(ctx)
		if err != nil {
			b.Fatal(err)
		}
	}
	requests := calls.Load() - before
	var want int64
	if refresh {
		want = int64(b.N)
	}
	if requests != want {
		b.Fatalf("got %d API requests for %d lookups, want %d", requests, b.N, want)
	}
	b.ReportMetric(float64(requests)/float64(b.N), "api-requests/op")
	benchmarkServerConfig = result
}
