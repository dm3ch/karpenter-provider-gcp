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

package provisioning

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/karpenter-gcp/karpenter-provider-gcp/test/pkg/environment"
)

// These specs cover how a Karpenter node becomes a Node object. GKE creates the
// Node either from the kubelet (VIA_KUBELET) or, with control plane node creation,
// from gcp-controller-manager (VIA_CONTROL_PLANE); see
// proposals/0016-control-plane-node-creation.md. Each registration spec skips itself
// unless the cluster is in the mode it covers, so a run exercises exactly the
// specs for its cluster and the report shows the other mode's spec as skipped.
var _ = Describe("Node creation mode", Label("suite:provisioning"), func() {
	It("registers Karpenter nodes when the kubelet creates the Node", func(ctx SpecContext) {
		env.SkipUnlessNodeCreationMode(ctx, gke.NodeCreationModeKubelet)
		node := runRegistrationTest(ctx)
		Expect(node.Annotations).NotTo(HaveKey(environment.NodeCreatedByAnnotation),
			"the kubelet creates the Node in this mode; the provider's node creation controller must not")
	}, SpecTimeout(20*time.Minute))

	It("registers Karpenter nodes when the control plane creates the Node", func(ctx SpecContext) {
		env.SkipUnlessNodeCreationMode(ctx, gke.NodeCreationModeControlPlane)
		node := runRegistrationTest(ctx)
		Expect(node.Annotations).To(HaveKeyWithValue(environment.NodeCreatedByAnnotation, environment.NodeCreatedByController),
			"GKE does not create Nodes for Karpenter VMs in this mode; the provider must")
	}, SpecTimeout(20*time.Minute))

	// Mode independent: a NodeClaim deleted before it registers must not leave a Node
	// or a VM behind, including when a Node is created for it around the deletion.
	It("leaves no Node or VM when the NodePool is deleted before registration", func(ctx SpecContext) {
		name, providerID := launchUnregisteredNodeClaim(ctx)
		GinkgoWriter.Printf("[action] deleting NodePool %s while %s is not registered yet\n", name, providerID)
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)

		Eventually(func(g Gomega) {
			claims, err := env.ListNodeClaimsForPool(ctx, name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(claims).To(BeEmpty(), "NodeClaims for deleted NodePool %s still exist", name)
		}).WithTimeout(environment.NodeCleanupTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())
		Expect(env.WaitForVMDeletion(ctx, providerID)).To(Succeed(), "VM for %s was not deleted", providerID)

		// A Node created for the claim just before deletion is removed by core's
		// NodeClaim finalizer or, failing that, by the cloud controller manager once
		// the instance is gone.
		Eventually(func(g Gomega) {
			nodes, err := env.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			for _, node := range nodes.Items {
				g.Expect(node.Spec.ProviderID).NotTo(Equal(providerID), "orphaned Node %s for deleted claim", node.Name)
			}
		}).WithTimeout(environment.NodeCleanupTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())
	}, SpecTimeout(20*time.Minute))
})

// runRegistrationTest provisions one node and asserts the registration contract that
// holds in every node creation mode: the NodeClaim's Node is Ready within the
// registration bound with no replacement claim, finishes Karpenter registration,
// carries GKE's readiness labels and runs every kube-system DaemonSet meant for it.
// It returns the Node for mode-specific assertions.
func runRegistrationTest(ctx context.Context) *corev1.Node {
	name := newNodeCreationName("register")
	setupNodeCreationPool(ctx, name)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})
	// Registered last so it runs first, before the cleanup above deletes the evidence.
	DeferCleanup(func(ctx context.Context) {
		if CurrentSpecReport().Failed() {
			env.DumpRegistrationDiagnostics(ctx, name)
		}
	})

	env.WaitForNodeClaimLaunched(ctx, name)
	// The bound covers registration and readiness, measured from NodeClaim creation,
	// and fails if the claim is replaced: a replacement that finally registers would
	// otherwise pass for a launch, time out and replace loop.
	node, claim := env.WaitForClaimNodeReady(ctx, name, environment.RegistrationBound)
	provisionedNodeName = node.Name
	GinkgoWriter.Printf("[result] node %s appeared %s and was Ready %s after NodeClaim %s\n", node.Name,
		node.CreationTimestamp.Sub(claim.GetCreationTimestamp().Time), time.Since(claim.GetCreationTimestamp().Time), claim.GetName())

	env.WaitForNodeClaimInitialized(ctx, name)
	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	environment.ExpectNodeRegistered(node, name)

	claims, err := env.ListNodeClaimsForPool(ctx, name)
	Expect(err).NotTo(HaveOccurred())
	Expect(claims).To(HaveLen(1), "NodeClaim was replaced after its Node registered")

	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).To(Equal(node.Name), "the workload must run on the NodeClaim's Node")

	env.ExpectGKEReadinessLabelsMatchManagedNode(ctx, node.Name)
	env.WaitForKubeSystemDaemonSetPodsReady(ctx, node.Name)
	env.WaitForDatapathAgentRunning(ctx, node.Name)
	return node
}

func newNodeCreationName(kind string) string {
	return environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "nodecreate", kind) + "-" + environment.UniqueSuffix()
}

// setupNodeCreationPool creates a NodeClass, NodePool and a Deployment that needs a
// node from the pool, registering cleanup of the pool objects.
func setupNodeCreationPool(ctx context.Context, name string) {
	GinkgoWriter.Printf("[setup] nodePool=%s\n", name)
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
	})
	env.CreateNodeClass(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
	})
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)
}

// launchUnregisteredNodeClaim sets up a pool and returns it once its NodeClaim has
// launched and neither registered nor got a Node, so the deletion that follows
// really happens before registration. A claim that registers before the check
// is discarded and the setup repeated; the spec fails rather than silently becoming
// an ordinary deletion test when the window is missed every time.
func launchUnregisteredNodeClaim(ctx context.Context) (name, providerID string) {
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		name = newNodeCreationName("delete")
		setupNodeCreationPool(ctx, name)
		env.WaitForNodeClaimLaunched(ctx, name)
		providerID = launchedProviderID(ctx, name)
		if env.NodeClaimUnregistered(ctx, name, providerID) {
			return name, providerID
		}
		GinkgoWriter.Printf("[retry] attempt %d/%d: %s registered before it could be deleted; trying a new pool\n", attempt, attempts, name)
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
	}
	Fail(fmt.Sprintf("the NodeClaim registered before the NodePool could be deleted in each of %d attempts, so deletion before registration was not exercised", attempts))
	return "", ""
}

// launchedProviderID returns the providerID of the pool's first launched NodeClaim.
func launchedProviderID(ctx context.Context, nodePool string) string {
	var providerID string
	Eventually(func(g Gomega) {
		claims, err := env.ListNodeClaimsForPool(ctx, nodePool)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(claims).NotTo(BeEmpty())
		providerID, _, _ = unstructured.NestedString(claims[0].Object, "status", "providerID")
		g.Expect(providerID).NotTo(BeEmpty(), "NodeClaim %s has no providerID yet", claims[0].GetName())
	}).WithTimeout(environment.NodeClaimLaunchTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())
	return providerID
}
