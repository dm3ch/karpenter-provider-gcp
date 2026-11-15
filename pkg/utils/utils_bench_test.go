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

import "testing"

var benchInstanceTypes = []struct {
	name     string
	cpuMCore int64
	memMiB   int64
}{
	{"e2-micro", 2000, 1024},
	{"e2-standard-4", 4000, 16384},
	{"n2-standard-8", 8000, 32768},
	{"c4-highmem-32", 32000, 253952},
	{"m3-ultramem-128", 128000, 3936256},
}

func BenchmarkResolveReservedResource(b *testing.B) {
	for i := 0; i < b.N; i++ {
		for _, it := range benchInstanceTypes {
			_, _, _, _, _ = ResolveReservedResource(it.name, it.cpuMCore, it.memMiB, 100, 750, 2)
		}
	}
}

func BenchmarkSanitizeGCELabelValue(b *testing.B) {
	inputs := []string{
		"karpenter.sh/nodepool=default",
		"Some Mixed_Case.Value/With:Special@Chars-and-a-very-long-suffix-that-exceeds-the-gce-limit-of-63-characters",
		"simple-value",
	}
	for i := 0; i < b.N; i++ {
		for _, s := range inputs {
			_ = SanitizeGCELabelValue(s)
		}
	}
}

func BenchmarkHash(b *testing.B) {
	s := "projects/test-project/zones/us-central1-a/instances/karpenter-default-abcde"
	for i := 0; i < b.N; i++ {
		_ = Hash(s)
	}
}
