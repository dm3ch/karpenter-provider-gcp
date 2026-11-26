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

package gke

import (
	"context"
	"fmt"

	containerv1 "google.golang.org/api/container/v1"
)

// NodeCreationMode is how GKE creates Node objects for a cluster.
type NodeCreationMode string

const (
	// NodeCreationModeKubelet is kubelet self-registration, the legacy behavior.
	NodeCreationModeKubelet NodeCreationMode = "VIA_KUBELET"
	// NodeCreationModeControlPlane means gcp-controller-manager creates the Node
	// after the kubelet CSR is approved and an admission controller rejects
	// kubelet Node creation.
	NodeCreationModeControlPlane NodeCreationMode = "VIA_CONTROL_PLANE"
)

// NodeCreationModeOf returns the cluster's node creation mode. Only an exact
// VIA_CONTROL_PLANE selects control plane creation; an absent config, an
// unspecified value or any value this provider does not know is the legacy
// kubelet mode, so an unrecognized future value never silently enables
// behavior meant for the control plane.
func NodeCreationModeOf(cluster *containerv1.Cluster) NodeCreationMode {
	if cluster != nil && cluster.NodeCreationConfig != nil &&
		cluster.NodeCreationConfig.NodeCreationMode == string(NodeCreationModeControlPlane) {
		return NodeCreationModeControlPlane
	}
	return NodeCreationModeKubelet
}

// ResolveNodeCreationMode returns the node creation mode of the provider's cluster
// from the cluster configuration the provider already caches. Callers must not
// cache the result: use Provider.InvalidateClusterConfig to force a fresh read.
func ResolveNodeCreationMode(ctx context.Context, p Provider) (NodeCreationMode, error) {
	cluster, err := p.GetClusterConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("reading node creation mode: %w", err)
	}
	return NodeCreationModeOf(cluster), nil
}
