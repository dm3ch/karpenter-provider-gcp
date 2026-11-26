/*
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

package events

import (
	corev1 "k8s.io/api/core/v1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
)

func NodePoolFailedToResolveNodeClass(nodePool *v1.NodePool) events.Event {
	return events.Event{
		InvolvedObject: nodePool,
		Type:           corev1.EventTypeWarning,
		Message:        "Failed resolving NodeClass",
		DedupeValues:   []string{string(nodePool.UID)},
	}
}

func NodeClaimFailedToResolveNodeClass(nodeClaim *v1.NodeClaim) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Message:        "Failed resolving NodeClass",
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}

// NodeClaimBootDiskTooSmall reports that GCE rejected the launch because the NodeClass boot disk is
// smaller than the image, which no retry fixes until the NodeClass is changed.
func NodeClaimBootDiskTooSmall(nodeClaim *v1.NodeClaim, err error) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Reason:         "BootDiskTooSmall",
		Message:        "The boot disk is smaller than the image; raise sizeGiB of the boot disk in the GCENodeClass spec.disks: " + err.Error(),
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}
