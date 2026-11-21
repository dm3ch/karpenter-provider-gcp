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
	"testing"

	"github.com/stretchr/testify/require"
	containerv1 "google.golang.org/api/container/v1"
)

func TestNodeCreationModeOf(t *testing.T) {
	for name, tc := range map[string]struct {
		cluster *containerv1.Cluster
		want    NodeCreationMode
	}{
		"nil cluster":        {nil, NodeCreationModeKubelet},
		"no config":          {&containerv1.Cluster{}, NodeCreationModeKubelet},
		"unspecified":        {cluster("NODE_CREATION_MODE_UNSPECIFIED"), NodeCreationModeKubelet},
		"kubelet":            {cluster("VIA_KUBELET"), NodeCreationModeKubelet},
		"control plane":      {cluster("VIA_CONTROL_PLANE"), NodeCreationModeControlPlane},
		"unknown future one": {cluster("VIA_SOMETHING_ELSE"), NodeCreationModeKubelet},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, NodeCreationModeOf(tc.cluster))
		})
	}
}

func cluster(mode string) *containerv1.Cluster {
	return &containerv1.Cluster{NodeCreationConfig: &containerv1.NodeCreationConfig{NodeCreationMode: mode}}
}
