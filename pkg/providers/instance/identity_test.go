/*
Copyright 2026 The Karpenter GCP Authors.

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

package instance

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/utils"
)

const identityPath = "/projects/test-project/zones/us-central1-a/instances/karpenter-node"

func writeIdentityInstance(w http.ResponseWriter, numericID uint64) {
	writeJSON(w, &compute.Instance{Name: "karpenter-node", Zone: "zones/us-central1-a", Id: numericID})
}

func serveNoInstance(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	writeJSON(w, &googleapi.Error{Code: http.StatusNotFound, Message: "not found"})
}

func TestGetIncarnation(t *testing.T) {
	t.Parallel()

	ownershipLabel := utils.SanitizeGCELabelValue(utils.LabelNodeClaimUIDKey)
	for _, tt := range []struct {
		name    string
		handler http.HandlerFunc
		// wantNil reports a missing VM, which is not an error.
		wantNil bool
		wantUID string
		wantErr bool
	}{
		{
			name: "live VM with the ownership label",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != identityPath {
					http.NotFound(w, r)
					return
				}
				writeJSON(w, &compute.Instance{Name: "karpenter-node", Id: 987654321, Labels: map[string]string{ownershipLabel: testClaimUID}})
			},
			wantUID: testClaimUID,
		},
		{
			name: "VM launched without the ownership label",
			handler: func(w http.ResponseWriter, r *http.Request) {
				writeIdentityInstance(w, 987654321)
			},
		},
		{name: "missing VM", handler: serveNoInstance, wantNil: true},
		{
			name: "temporary Compute failure",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				writeJSON(w, &googleapi.Error{Code: http.StatusServiceUnavailable, Message: "backend unavailable"})
			},
			wantErr: true,
		},
		{
			name: "VM without a numeric ID",
			handler: func(w http.ResponseWriter, r *http.Request) {
				writeIdentityInstance(w, 0)
			},
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var requests []string
			var mu sync.Mutex
			provider := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.URL.Path)
				mu.Unlock()
				tt.handler(w, r)
			}))
			// A stale name-keyed entry must not satisfy the fresh read.
			provider.instanceCache.Set("karpenter-node", &Instance{Name: "karpenter-node", NumericID: 1}, 0)

			incarnation, err := provider.GetIncarnation(context.Background(), "us-central1-a", "karpenter-node")
			require.Equal(t, []string{identityPath}, requests,
				"one Compute instance read, with no attestation key or project number lookup")
			if tt.wantErr {
				require.Error(t, err)
				require.Nil(t, incarnation)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				require.Nil(t, incarnation)
				return
			}
			require.Equal(t, "test-project", incarnation.ProjectID)
			require.Equal(t, "us-central1-a", incarnation.Zone)
			require.Equal(t, "karpenter-node", incarnation.Name)
			require.Equal(t, uint64(987654321), incarnation.NumericID)
			require.Equal(t, tt.wantUID, incarnation.NodeClaimUID)
		})
	}
}
