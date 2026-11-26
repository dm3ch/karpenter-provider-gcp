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

package version

import "testing"

func TestValidateK8sVersion(t *testing.T) {
	for v, wantErr := range map[string]bool{
		"1.28.1":                     false,
		"1.37.1":                     false,
		"1.37.99":                    false,
		"1.37.2-gke.1606000":         false,
		"1.28.0":                     true,
		"1.38.0":                     true,
		"1.38.0-gke.5166000+preview": true,
	} {
		if err := validateK8sVersion(v); (err != nil) != wantErr {
			t.Errorf("validateK8sVersion(%q) error = %v, want error %v", v, err, wantErr)
		}
	}
}
