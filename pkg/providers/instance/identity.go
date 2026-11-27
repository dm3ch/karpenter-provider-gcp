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
	"fmt"

	"google.golang.org/api/compute/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/utils"
)

// InstanceIncarnation is the freshly read VM incarnation and NodeClaim ownership
// association used for CSR approval that carries no TPM attestation. It is an
// association under trusted provisioning IAM, not proof of who requested a
// certificate.
type InstanceIncarnation struct {
	// ProjectID is the configured project name the lookup was made in.
	ProjectID string
	Zone      string
	Name      string
	// NumericID is the numeric Compute Engine instance ID of the live VM.
	NumericID uint64
	// NodeClaimUID is the NodeClaim UID recorded in the VM's ownership label, or
	// empty when the VM was launched without it.
	NodeClaimUID string
}

// GetIncarnation returns the live numeric ID and NodeClaim ownership label of the
// VM with this name in the configured project and zone. Like GetIdentity it reads
// the Compute API fresh instead of the name-keyed instance cache, but it needs no
// attestation key. A VM that does not exist returns (nil, nil).
func (p *DefaultProvider) GetIncarnation(ctx context.Context, zone, name string) (*InstanceIncarnation, error) {
	live, err := p.getInstance(ctx, zone, name)
	if err != nil || live == nil {
		return nil, err
	}
	if live.Id == 0 {
		return nil, fmt.Errorf("instance %q has no numeric instance ID", name)
	}
	return &InstanceIncarnation{
		ProjectID:    p.projectID,
		Zone:         zone,
		Name:         name,
		NumericID:    live.Id,
		NodeClaimUID: live.Labels[utils.SanitizeGCELabelValue(utils.LabelNodeClaimUIDKey)],
	}, nil
}

func (p *DefaultProvider) getInstance(ctx context.Context, zone, name string) (*compute.Instance, error) {
	instance, err := p.computeService.Instances.Get(p.projectID, zone, name).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting instance: %w", err)
	}
	return instance, nil
}
