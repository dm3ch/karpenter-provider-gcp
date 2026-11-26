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

package environment

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/instance"
)

var (
	// NodeCreatedByAnnotation marks a Node created by the provider's node creation
	// controller (proposals/0016-control-plane-node-creation.md). It is absent on
	// Nodes the kubelet or GKE's control plane created.
	NodeCreatedByAnnotation = gcpv1alpha1.AnnotationNodeCreatedBy
	// NodeCreatedByController is the NodeCreatedByAnnotation value for that controller.
	NodeCreatedByController = gcpv1alpha1.NodeCreatedByController
)

const (
	// RegistrationBound is the longest healthy registration the specs accept: from
	// NodeClaim creation until its Node is Ready. It sits far below Karpenter's
	// 15-minute registration timeout so a launch, time out and replace loop fails the
	// spec instead of passing after a replacement finally registers.
	RegistrationBound = 5 * time.Minute
)

// NodeCreationMode returns the cluster's node creation mode. It fails the spec
// while a cluster operation is in progress, because GKE keeps reporting the
// previous mode until a mode change finishes and a wrong answer here would run
// or skip the wrong specs without any signal.
func (e *Environment) NodeCreationMode(ctx context.Context) gke.NodeCreationMode {
	cluster, err := e.GetCluster(ctx)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "getting the cluster to read its node creation mode")
	ExpectWithOffset(1, cluster.Status).To(Equal("RUNNING"),
		"cluster is %s; its reported node creation mode is unreliable until the operation finishes", cluster.Status)
	return gke.NodeCreationModeOf(cluster)
}

// SkipUnlessNodeCreationMode skips the running spec unless the cluster uses want.
func (e *Environment) SkipUnlessNodeCreationMode(ctx context.Context, want gke.NodeCreationMode) {
	got := e.NodeCreationMode(ctx)
	GinkgoWriter.Printf("[setup] cluster node creation mode: %s (spec requires %s)\n", got, want)
	if got != want {
		Skip(fmt.Sprintf("cluster node creation mode is %s; this spec requires %s", got, want))
	}
}

// ExpectGKEReadinessLabelsMatchManagedNode asserts that every GKE readiness label
// present on a GKE-managed node of the cluster carries the same value on nodeName.
// Labels the managed node lacks are not required: they depend on cluster add-ons.
// Without a managed reference node the comparison is skipped and logged.
func (e *Environment) ExpectGKEReadinessLabelsMatchManagedNode(ctx context.Context, nodeName string) {
	nodes, err := e.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "listing nodes")

	var reference, candidate *corev1.Node
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Name == nodeName {
			candidate = node
			continue
		}
		_, karpenterNode := node.Labels[karpv1.NodePoolLabelKey]
		if reference == nil && !karpenterNode && node.Labels["cloud.google.com/gke-nodepool"] != "" && IsNodeReady(node) {
			reference = node
		}
	}
	ExpectWithOffset(1, candidate).NotTo(BeNil(), "node %s not found", nodeName)
	if reference == nil {
		GinkgoWriter.Printf("[check] no Ready GKE-managed node to compare readiness labels against; skipping the comparison\n")
		return
	}
	for _, key := range gcpv1alpha1.GKEReadinessLabelKeys {
		want, ok := reference.Labels[key]
		if !ok {
			continue
		}
		ExpectWithOffset(1, candidate.Labels).To(HaveKeyWithValue(key, want),
			"GKE readiness label %s differs from managed node %s", key, reference.Name)
	}
}

// WaitForKubeSystemDaemonSetPodsReady waits until every kube-system DaemonSet
// that is meant to run on nodeName has a Running and Ready pod there: the GKE
// agents (datapath, metadata server, metrics, CSI) a node needs before it can
// serve workloads. A DaemonSet is meant to run on the node when its node selector,
// required node affinity and tolerations admit it, so an agent that never gets a
// pod fails the wait instead of being missed because its siblings are Ready. A
// node that registers without the labels GKE expects is not caught here, because
// the DaemonSets that select on them stop applying to it;
// ExpectGKEReadinessLabelsMatchManagedNode covers that.
func (e *Environment) WaitForKubeSystemDaemonSetPodsReady(ctx context.Context, nodeName string) {
	Eventually(func(g Gomega) {
		node, err := e.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred(), "getting node %s", nodeName)
		daemonSets, err := e.KubeClient.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
		g.Expect(err).NotTo(HaveOccurred(), "listing kube-system DaemonSets")
		pods, err := e.KubeClient.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		g.Expect(err).NotTo(HaveOccurred(), "listing kube-system pods on node %s", nodeName)

		podByDaemonSet := map[types.UID]*corev1.Pod{}
		for i := range pods.Items {
			for _, owner := range pods.Items[i].OwnerReferences {
				if owner.Kind == "DaemonSet" {
					podByDaemonSet[owner.UID] = &pods.Items[i]
				}
			}
		}

		var expected []string
		for i := range daemonSets.Items {
			ds := &daemonSets.Items[i]
			meant, err := daemonSetMeantForNode(ds, node)
			g.Expect(err).NotTo(HaveOccurred(), "evaluating DaemonSet %s against node %s", ds.Name, nodeName)
			if !meant {
				continue
			}
			expected = append(expected, ds.Name)
			pod := podByDaemonSet[ds.UID]
			g.Expect(pod).NotTo(BeNil(), "kube-system DaemonSet %s is meant to run on %s but has no pod there", ds.Name, nodeName)
			g.Expect(isRunningAndReady(pod)).To(BeTrue(),
				"kube-system DaemonSet pod %s on %s is not Running/Ready (phase=%s)", pod.Name, nodeName, pod.Status.Phase)
		}
		g.Expect(expected).NotTo(BeEmpty(), "no kube-system DaemonSet applies to node %s", nodeName)
	}).WithTimeout(ProvisioningTimeout).WithPolling(DefaultPollInterval).Should(Succeed())
}

// daemonSetAutoTolerated are the taints the DaemonSet controller tolerates on
// every DaemonSet pod without them appearing in the pod template.
var daemonSetAutoTolerated = map[string]struct{}{
	corev1.TaintNodeNotReady:           {},
	corev1.TaintNodeUnreachable:        {},
	corev1.TaintNodeDiskPressure:       {},
	corev1.TaintNodeMemoryPressure:     {},
	corev1.TaintNodePIDPressure:        {},
	corev1.TaintNodeUnschedulable:      {},
	corev1.TaintNodeNetworkUnavailable: {},
}

// daemonSetMeantForNode reports whether the DaemonSet controller would run a pod
// of ds on node: the pod template's node selector and required node affinity
// match the node's labels, and its tolerations cover every scheduling taint the
// DaemonSet controller does not tolerate itself.
func daemonSetMeantForNode(ds *appsv1.DaemonSet, node *corev1.Node) (bool, error) {
	spec := ds.Spec.Template.Spec
	matches, err := nodeaffinity.GetRequiredNodeAffinity(&corev1.Pod{Spec: spec}).Match(node)
	if err != nil || !matches {
		return false, err
	}
	for i := range node.Spec.Taints {
		taint := &node.Spec.Taints[i]
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if _, ok := daemonSetAutoTolerated[taint.Key]; ok {
			continue
		}
		if !slices.ContainsFunc(spec.Tolerations, func(t corev1.Toleration) bool { return t.ToleratesTaint(klog.Background(), taint, false) }) {
			return false, nil
		}
	}
	return true, nil
}

// ListNodeClaimsForPool lists the NodeClaims owned by nodePool.
func (e *Environment) ListNodeClaimsForPool(ctx context.Context, nodePool string) ([]unstructured.Unstructured, error) {
	list, err := e.DynamicClient.Resource(nodeClaimGVR).List(ctx, metav1.ListOptions{LabelSelector: karpv1.NodePoolLabelKey + "=" + nodePool})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// nodeForProviderID returns the Node whose spec.providerID is providerID, or nil.
// The API server cannot select Nodes by provider ID, so it filters a full list.
func (e *Environment) nodeForProviderID(ctx context.Context, providerID string) (*corev1.Node, error) {
	nodes, err := e.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range nodes.Items {
		if nodes.Items[i].Spec.ProviderID == providerID {
			return &nodes.Items[i], nil
		}
	}
	return nil, nil
}

// WaitForClaimNodeReady waits for the pool's first NodeClaim to get a Ready Node
// and returns both. The wait ends bound after the NodeClaim was created, so a
// Node that appears promptly but takes long to become Ready fails like a slow
// registration, and it fails at once if the NodeClaim is replaced: a replacement
// that finally registers must not pass for the claim that was timed.
func (e *Environment) WaitForClaimNodeReady(ctx context.Context, nodePool string, bound time.Duration) (*corev1.Node, *unstructured.Unstructured) {
	var first *unstructured.Unstructured
	Eventually(ctx, func(g Gomega) {
		claims, err := e.ListNodeClaimsForPool(ctx, nodePool)
		g.Expect(err).NotTo(HaveOccurred(), "listing NodeClaims for pool %s", nodePool)
		g.Expect(claims).NotTo(BeEmpty(), "no NodeClaim for pool %s yet", nodePool)
		first = claims[0].DeepCopy()
	}).WithTimeout(NodeClaimLaunchTimeout).WithPolling(DefaultPollInterval).Should(Succeed())

	var node *corev1.Node
	var claim *unstructured.Unstructured
	remaining := max(time.Until(first.GetCreationTimestamp().Add(bound)), time.Second)
	Eventually(ctx, func(g Gomega) {
		claims, err := e.ListNodeClaimsForPool(ctx, nodePool)
		g.Expect(err).NotTo(HaveOccurred(), "listing NodeClaims for pool %s", nodePool)
		if len(claims) != 1 || claims[0].GetName() != first.GetName() {
			StopTrying(fmt.Sprintf("NodeClaim %s was replaced before its Node became Ready", first.GetName())).Now()
		}
		claim = &claims[0]

		providerID, _, _ := unstructured.NestedString(claim.Object, "status", "providerID")
		g.Expect(providerID).NotTo(BeEmpty(), "NodeClaim %s has no providerID yet", claim.GetName())
		node, err = e.nodeForProviderID(ctx, providerID)
		g.Expect(err).NotTo(HaveOccurred(), "listing nodes")
		g.Expect(node).NotTo(BeNil(), "no Node for NodeClaim %s yet", claim.GetName())
		g.Expect(IsNodeReady(node)).To(BeTrue(), "Node %s is not Ready yet", node.Name)
	}).WithTimeout(remaining).WithPolling(DefaultPollInterval).Should(Succeed(),
		"the NodeClaim's Node was not Ready within %s of the NodeClaim's creation", bound)
	return node, claim
}

// NodeClaimUnregistered reports whether the pool's NodeClaims have neither
// registered nor got a Node for providerID, that is, whether deleting the pool now
// still exercises deletion before registration.
func (e *Environment) NodeClaimUnregistered(ctx context.Context, nodePool, providerID string) bool {
	claims, err := e.ListNodeClaimsForPool(ctx, nodePool)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "listing NodeClaims for pool %s", nodePool)
	for i := range claims {
		if nodeName, _, _ := unstructured.NestedString(claims[i].Object, "status", "nodeName"); nodeName != "" {
			return false
		}
		conditions, _, _ := unstructured.NestedSlice(claims[i].Object, "status", "conditions")
		for _, c := range conditions {
			if cond, ok := c.(map[string]any); ok && cond["type"] == "Registered" && cond["status"] == "True" {
				return false
			}
		}
	}
	node, err := e.nodeForProviderID(ctx, providerID)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "listing nodes")
	return node == nil
}

// ExpectNodeRegistered asserts the Node finished Karpenter registration: Ready,
// no leftover registration or cloud-initialization taint, registered and
// initialized labels, and the NodePool label.
func ExpectNodeRegistered(node *corev1.Node, nodePool string) {
	ExpectWithOffset(1, IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	ExpectWithOffset(1, node.Labels).To(HaveKeyWithValue(karpv1.NodeRegisteredLabelKey, "true"))
	ExpectWithOffset(1, node.Labels).To(HaveKeyWithValue(karpv1.NodeInitializedLabelKey, "true"))
	ExpectWithOffset(1, node.Labels).To(HaveKeyWithValue(karpv1.NodePoolLabelKey, nodePool))
	for _, taint := range node.Spec.Taints {
		ExpectWithOffset(1, taint.Key).NotTo(Equal(karpv1.UnregisteredTaintKey), "registration taint left on node %s", node.Name)
		ExpectWithOffset(1, taint.Key).NotTo(Equal(instance.UninitializedTaintKey), "cloud initialization taint left on node %s", node.Name)
	}
}

// DumpRegistrationDiagnostics prints the evidence needed to tell why a Karpenter
// node did not register: NodeClaim conditions, the kubelet client CSRs for the
// instance with their approver, Node and kubelet events, and the cluster's
// node creation mode. It never fails the spec.
func (e *Environment) DumpRegistrationDiagnostics(ctx context.Context, nodePool string) {
	w := GinkgoWriter
	fmt.Fprintf(w, "\n[diagnostics] registration state for NodePool %s\n", nodePool)
	e.dumpClusterMode(ctx)
	instances := e.dumpNodeClaims(ctx, nodePool)
	e.dumpCSRs(ctx, instances)
	e.dumpInstanceEvents(ctx, instances)
}

func (e *Environment) dumpClusterMode(ctx context.Context) {
	cluster, err := e.GetCluster(ctx)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "[diagnostics] reading cluster: %v\n", err)
		return
	}
	fmt.Fprintf(GinkgoWriter, "[diagnostics] cluster status=%s version=%s node creation mode=%s\n",
		cluster.Status, cluster.CurrentMasterVersion, gke.NodeCreationModeOf(cluster))
}

// dumpNodeClaims prints the pool's NodeClaims and returns the instance names they launched.
func (e *Environment) dumpNodeClaims(ctx context.Context, nodePool string) map[string]struct{} {
	w := GinkgoWriter
	instances := map[string]struct{}{}
	claims, err := e.ListNodeClaimsForPool(ctx, nodePool)
	if err != nil {
		fmt.Fprintf(w, "[diagnostics] listing NodeClaims: %v\n", err)
		return instances
	}
	for i := range claims {
		claim := &claims[i]
		providerID, _, _ := unstructured.NestedString(claim.Object, "status", "providerID")
		nodeName, _, _ := unstructured.NestedString(claim.Object, "status", "nodeName")
		fmt.Fprintf(w, "[diagnostics] NodeClaim %s created=%s providerID=%s nodeName=%q\n",
			claim.GetName(), claim.GetCreationTimestamp().Format(time.RFC3339), providerID, nodeName)
		conditions, _, _ := unstructured.NestedSlice(claim.Object, "status", "conditions")
		for _, c := range conditions {
			if cond, ok := c.(map[string]any); ok {
				fmt.Fprintf(w, "[diagnostics]   %v=%v reason=%v message=%v\n", cond["type"], cond["status"], cond["reason"], cond["message"])
			}
		}
		if _, _, name, err := parseProviderID(providerID); err == nil {
			instances[name] = struct{}{}
		}
	}
	return instances
}

func (e *Environment) dumpCSRs(ctx context.Context, instances map[string]struct{}) {
	w := GinkgoWriter
	csrs, err := e.KubeClient.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(w, "[diagnostics] listing CSRs: %v\n", err)
		return
	}
	for i := range csrs.Items {
		csr := &csrs.Items[i]
		name := csrSubjectNode(csr)
		if _, ok := instances[name]; !ok {
			continue
		}
		fmt.Fprintf(w, "[diagnostics] CSR %s node=%s requester=%s created=%s\n", csr.Name, name, csr.Spec.Username, csr.CreationTimestamp.Format(time.RFC3339))
		for _, c := range csr.Status.Conditions {
			fmt.Fprintf(w, "[diagnostics]   %s reason=%s message=%s\n", c.Type, c.Reason, c.Message)
		}
		if len(csr.Status.Conditions) == 0 {
			fmt.Fprintf(w, "[diagnostics]   pending\n")
		}
	}
}

func (e *Environment) dumpInstanceEvents(ctx context.Context, instances map[string]struct{}) {
	w := GinkgoWriter
	for name := range instances {
		if _, err := e.KubeClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{}); err != nil {
			fmt.Fprintf(w, "[diagnostics] node %s: %v\n", name, err)
		}
		events, err := e.KubeClient.CoreV1().Events(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + name})
		if err != nil {
			fmt.Fprintf(w, "[diagnostics] listing events for %s: %v\n", name, err)
			continue
		}
		for _, ev := range events.Items {
			fmt.Fprintf(w, "[diagnostics] event %s %s %s: %s\n", ev.LastTimestamp.Format(time.RFC3339), ev.Type, ev.Reason, truncate(ev.Message, 300))
		}
	}
}

// csrSubjectNode returns the node name a kubelet client CSR requests, or "".
func csrSubjectNode(csr *certv1.CertificateSigningRequest) string {
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil {
		return ""
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(req.Subject.CommonName, "system:node:")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
