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
	"fmt"
	"testing"

	containerv1 "google.golang.org/api/container/v1"
)

func BenchmarkResolveVersionForChannel(b *testing.B) {
	var valid []string
	for minor := 30; minor <= 35; minor++ {
		for patch := 0; patch < 10; patch++ {
			valid = append(valid, fmt.Sprintf("1.%d.%d-gke.%d000", minor, patch, 1000+patch))
		}
	}
	sc := &containerv1.ServerConfig{Channels: []*containerv1.ReleaseChannelConfig{
		{Channel: "RAPID", DefaultVersion: "1.35.1-gke.1000000", ValidVersions: valid},
		{Channel: "REGULAR", DefaultVersion: "1.34.1-gke.1000000", ValidVersions: valid},
		{Channel: "STABLE", DefaultVersion: "1.33.1-gke.1000000", ValidVersions: valid},
	}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ResolveVersionForChannel(sc, "stable", "v1.32.4"); err != nil {
			b.Fatal(err)
		}
	}
}
