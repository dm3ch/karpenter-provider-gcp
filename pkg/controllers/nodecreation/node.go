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
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/instance"
)

// kubeletLabelKeys are the well-known labels in kubernetes.io namespaces that a
// kubelet may set on its own Node (the NodeRestriction admission plugin allows
// them), including the legacy beta and failure-domain spellings.
var kubeletLabelKeys = map[string]struct{}{
	"kubernetes.io/hostname":                   {},
	"kubernetes.io/arch":                       {},
	"kubernetes.io/os":                         {},
	"topology.kubernetes.io/region":            {},
	"topology.kubernetes.io/zone":              {},
	"failure-domain.beta.kubernetes.io/region": {},
	"failure-domain.beta.kubernetes.io/zone":   {},
	"beta.kubernetes.io/arch":                  {},
	"beta.kubernetes.io/os":                    {},
	"beta.kubernetes.io/instance-type":         {},
	"node.kubernetes.io/instance-type":         {},
}

// kubeletLabelDomains are namespaces under which a kubelet may set any label.
var kubeletLabelDomains = []string{"kubelet.kubernetes.io", "node.kubernetes.io"}

// nodeLabelsOf reads the label set the provider recorded on the NodeClaim at
// launch. The annotation is untrusted input, since anyone who can patch NodeClaims
// can edit it: it must be present, bounded and made of valid labels.
func nodeLabelsOf(nc *karpv1.NodeClaim) (map[string]string, error) {
	raw, ok := nc.Annotations[v1alpha1.AnnotationNodeLabels]
	if !ok {
		return nil, fmt.Errorf("annotation is missing")
	}
	if len(raw) > maxNodeLabelsBytes {
		return nil, fmt.Errorf("annotation is %d bytes, over the %d byte limit", len(raw), maxNodeLabelsBytes)
	}
	return parseLabels(raw)
}

// parseLabels parses the comma-separated key=value list of the annotation. Every
// key and value must be a valid Kubernetes label, so a Node built from the result
// cannot be rejected for a malformed label that was silently skipped.
func parseLabels(raw string) (map[string]string, error) {
	labels := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return labels, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			return nil, fmt.Errorf("label entry %q is not key=value", pair)
		}
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return nil, fmt.Errorf("label key %q is invalid: %s", key, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return nil, fmt.Errorf("label value of %q is invalid: %s", key, strings.Join(errs, "; "))
		}
		labels[key] = value
	}
	return labels, nil
}

// renderNode builds the Node a kubelet would have registered for a launched
// instance. It carries the labels the provider recorded on the NodeClaim, the launch taints plus the cloud initialization taint, and the
// provider ID the core registration controller matches on. It deliberately has
// no owner reference or finalizer: core adds them when registration completes.
// dropped lists the label keys the policy removed.
func renderNode(name, providerID string, nodeClass *v1alpha1.GCENodeClass, kubeLabels map[string]string) (node *corev1.Node, dropped []string) {
	labels, dropped := filterNodeLabels(kubeLabels)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      labels,
			Annotations: map[string]string{v1alpha1.AnnotationNodeCreatedBy: v1alpha1.NodeCreatedByController},
		},
		Spec: corev1.NodeSpec{
			ProviderID: providerID,
			Taints:     instance.NodeRegistrationTaints(nodeClass, kubeLabels),
		},
	}, dropped
}

// filterNodeLabels applies the label policy to the recorded label set.
//
// The controller is a trusted control plane component, so unlike a kubelet it may
// set node-restriction labels (for example secondary boot disk labels). It must
// not be more permissive than the data it reads, which anyone who can patch
// NodeClaims can write: labels in the kubernetes.io and k8s.io
// namespaces other than those a kubelet may self-assign (node-role among them)
// are dropped, as are the registration labels that only core may add.
func filterNodeLabels(in map[string]string) (kept map[string]string, dropped []string) {
	kept = make(map[string]string, len(in))
	for key, value := range in {
		if labelAllowed(key) {
			kept[key] = value
		} else {
			dropped = append(dropped, key)
		}
	}
	sort.Strings(dropped)
	return kept, dropped
}

func labelAllowed(key string) bool {
	// Core adds these when registration and initialization complete. Their absence
	// before then is what registration's race protection relies on.
	if key == karpv1.NodeRegisteredLabelKey || key == karpv1.NodeInitializedLabelKey {
		return false
	}
	domain, _, hasPrefix := strings.Cut(key, "/")
	if !hasPrefix || !isReservedDomain(domain) {
		return true
	}
	if domain == "node-restriction.kubernetes.io" || strings.HasSuffix(domain, ".node-restriction.kubernetes.io") {
		return true
	}
	if _, ok := kubeletLabelKeys[key]; ok {
		return true
	}
	for _, allowed := range kubeletLabelDomains {
		if domain == allowed || strings.HasSuffix(domain, "."+allowed) {
			return true
		}
	}
	return false
}

func isReservedDomain(domain string) bool {
	for _, reserved := range []string{"kubernetes.io", "k8s.io"} {
		if domain == reserved || strings.HasSuffix(domain, "."+reserved) {
			return true
		}
	}
	return false
}
