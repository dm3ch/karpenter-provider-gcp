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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/gke"
)

const (
	// NodeCreatedByAnnotation marks a Node created by the provider's node creation
	// controller (proposals/0014-control-plane-node-creation.md). It is absent on
	// Nodes the kubelet or GKE's control plane created.
	NodeCreatedByAnnotation = "karpenter.k8s.gcp/created-by"
	// NodeCreatedByController is the NodeCreatedByAnnotation value for that controller.
	NodeCreatedByController = "nodecreation-controller"

	// uninitializedTaintKey is added to Nodes of an external cloud provider until
	// the cloud controller manager initializes them.
	uninitializedTaintKey = "node.cloudprovider.kubernetes.io/uninitialized"

	// RegistrationBound is the longest healthy Node registration the specs accept,
	// measured from NodeClaim creation. It sits far below Karpenter's 15-minute
	// registration timeout so a launch, time out and replace loop fails the spec
	// instead of passing after a replacement finally registers.
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

// WaitForKubeSystemDaemonSetPodsReady waits until at least one kube-system
// DaemonSet pod runs on nodeName and every such pod is Running and Ready: the
// GKE agents (datapath, metadata server, metrics, CSI) that a node needs before
// it can serve workloads. A node that registers without the labels, taints or
// cloud initialization GKE expects leaves some of them unscheduled or failing.
func (e *Environment) WaitForKubeSystemDaemonSetPodsReady(ctx context.Context, nodeName string) {
	Eventually(func(g Gomega) {
		pods, err := e.KubeClient.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		g.Expect(err).NotTo(HaveOccurred(), "listing kube-system pods on node %s", nodeName)

		var daemonSetPods []corev1.Pod
		for _, pod := range pods.Items {
			for _, owner := range pod.OwnerReferences {
				if owner.Kind == "DaemonSet" {
					daemonSetPods = append(daemonSetPods, pod)
					break
				}
			}
		}
		g.Expect(daemonSetPods).NotTo(BeEmpty(), "no kube-system DaemonSet pod on node %s yet", nodeName)
		for i := range daemonSetPods {
			g.Expect(isRunningAndReady(&daemonSetPods[i])).To(BeTrue(),
				"kube-system DaemonSet pod %s on %s is not Running/Ready (phase=%s)",
				daemonSetPods[i].Name, nodeName, daemonSetPods[i].Status.Phase)
		}
	}).WithTimeout(ProvisioningTimeout).WithPolling(DefaultPollInterval).Should(Succeed())
}

// ListNodeClaimsForPool lists the NodeClaims owned by nodePool.
func (e *Environment) ListNodeClaimsForPool(ctx context.Context, nodePool string) ([]unstructured.Unstructured, error) {
	list, err := e.DynamicClient.Resource(nodeClaimGVR).List(ctx, metav1.ListOptions{LabelSelector: karpv1.NodePoolLabelKey + "=" + nodePool})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
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
		ExpectWithOffset(1, taint.Key).NotTo(Equal(uninitializedTaintKey), "cloud initialization taint left on node %s", node.Name)
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
