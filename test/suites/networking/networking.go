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

package networking

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/test/pkg/environment"
)

var env *environment.Environment
var _ = BeforeEach(func() { env = environment.Current() })

var _ = DescribeTable("Networking on private nodes", Label("suite:networking"),
	func(ctx SpecContext, networkConfig map[string]any, wantCanIPForward bool) {
		runPrivateNodeTest(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		}, networkConfig, wantCanIPForward)
	},
	Entry("provisions a node without an external IP and leaves IP forwarding disabled by default",
		map[string]any{"enablePrivateNodes": true}, false, SpecTimeout(15*time.Minute)),
	Entry("enables IP forwarding on the GCE instance when canIpForward is true",
		map[string]any{"enablePrivateNodes": true, "canIpForward": true}, true, SpecTimeout(15*time.Minute)),
)
