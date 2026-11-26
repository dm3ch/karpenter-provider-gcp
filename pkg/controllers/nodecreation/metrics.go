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

package nodecreation

import (
	"github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	karpmetrics "sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	resultCreated   = "created"
	resultExists    = "exists"
	resultCollision = "collision"
	resultError     = "error"
)

// nodeCreationTotal counts Node creation attempts by outcome.
var nodeCreationTotal = metrics.NewPrometheusCounter(
	crmetrics.Registry,
	prometheus.CounterOpts{
		Namespace: karpmetrics.Namespace,
		Subsystem: "gcp",
		Name:      "nodecreation_total",
		Help:      "Number of Nodes the provider tried to create for enrolled NodeClaims, by result.",
	},
	[]metrics.Label{{
		Name: "result",
		Help: "Outcome of the attempt.",
		Values: []metrics.Value{
			{Name: resultCreated, Help: "The Node was created."},
			{Name: resultExists, Help: "A Node with the expected name and provider ID already existed."},
			{Name: resultCollision, Help: "A different Node already owns the name or provider ID; nothing was changed."},
			{Name: resultError, Help: "Creation failed and will be retried."},
		},
	}},
	metrics.Beta,
)
