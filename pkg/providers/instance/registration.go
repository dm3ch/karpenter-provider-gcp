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

package instance

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// UninitializedTaintKey is the taint an external cloud provider's Nodes carry
// until the cloud controller manager initializes them. A Node the kubelet
// registers gets it from --cloud-provider=external; a Node created by the
// provider must carry it too, or the cloud controller manager never sets the
// Node's addresses and GKE's datapath agent fails with "host IP unknown".
const UninitializedTaintKey = "node.cloudprovider.kubernetes.io/uninitialized"

// NodeRegistrationTaints returns the taints a Node created for a launched
// instance carries before core registers it: the same launch taints the VM's
// kubelet would register with (unregistered, plus the GPU taint when the
// NodeClass asks for one on a GPU instance) and the uninitialized taint.
// kubeLabels is the instance's kube-labels item; GPU instances are the ones
// that carry the GPU driver version label.
func NodeRegistrationTaints(nodeClass *v1alpha1.GCENodeClass, kubeLabels map[string]string) []corev1.Taint {
	taints := []corev1.Taint{
		{Key: karpv1.UnregisteredTaintKey, Value: "true", Effect: corev1.TaintEffectNoExecute},
		{Key: UninitializedTaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule},
	}
	if _, isGPU := kubeLabels[v1alpha1.LabelGKEGPUDriverVersion]; isGPU {
		taints = append(taints, gpuTaints(nodeClass)...)
	}
	return taints
}

// ParseProviderID splits a GCE provider ID, gce://<project>/<zone>/<instance>,
// into its parts. It rejects any other shape and empty parts.
func ParseProviderID(providerID string) (project, zone, name string, err error) {
	project, zone, name, err = parseGCEProviderID(providerID)
	if err == nil && (project == "" || zone == "" || name == "") {
		return "", "", "", fmt.Errorf("invalid GCE providerID %q: empty project, zone or instance", providerID)
	}
	return project, zone, name, err
}
