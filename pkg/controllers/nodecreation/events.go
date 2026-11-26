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
	"time"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
)

const (
	reasonNodeCreated      = "NodeCreatedByController"
	reasonSkipped          = "NodeCreationSkipped"
	reasonNodeNameConflict = "NodeNameCollision"
)

func nodeCreatedEvent(nc *karpv1.NodeClaim, nodeName string) events.Event {
	return events.Event{
		InvolvedObject: nc,
		Type:           corev1.EventTypeNormal,
		Reason:         reasonNodeCreated,
		Message:        fmt.Sprintf("Created Node %s for the enrolled instance; the kubelet adopts it and registration continues", nodeName),
		DedupeValues:   []string{string(nc.UID)},
	}
}

func skippedEvent(nc *karpv1.NodeClaim, why string) events.Event {
	return events.Event{
		InvolvedObject: nc,
		Type:           corev1.EventTypeWarning,
		Reason:         reasonSkipped,
		Message:        why,
		DedupeValues:   []string{string(nc.UID), why},
		DedupeTimeout:  5 * time.Minute,
	}
}

func nodeNameConflictEvent(nc *karpv1.NodeClaim, why string) events.Event {
	return events.Event{
		InvolvedObject: nc,
		Type:           corev1.EventTypeWarning,
		Reason:         reasonNodeNameConflict,
		Message:        why,
		DedupeValues:   []string{string(nc.UID), why},
		DedupeTimeout:  5 * time.Minute,
	}
}
