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

// Package nodecreation creates the Node for a launched, enrolled NodeClaim on
// GKE clusters where the control plane will not (VIA_CONTROL_PLANE node creation
// does not cover VMs outside GKE's own node pools). The kubelet then adopts the
// Node and core Karpenter registration continues unchanged. See
// proposals/0016-control-plane-node-creation.md.
package nodecreation

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/instance"
)

// DefaultGrace is how long after enrollment a claim may stay without a Node before
// the controller re-reads the cluster's node creation mode. The cached mode can
// lag a mode change by the cluster-config cache TTL (30 minutes), so the re-read is
// what lets a claim launched after a switch to VIA_CONTROL_PLANE get its Node.
//
// Five minutes sits between two bounds. Below it, the kubelet registers about a
// second after its certificate and a Node is Ready within about a minute of
// the claim's launch, so any delay past a minute never races a healthy kubelet.
// Above it, core deletes a NodeClaim that has not registered 15 minutes after its
// creation, and enrollment takes about 1.5 minutes from creation: re-reading at
// five minutes still leaves roughly 8 minutes to create the Node and register.
const DefaultGrace = 5 * time.Minute

// maxNodeLabelsBytes bounds the node-labels annotation the controller parses. The
// set the provider writes is about 1 KB; the bound only limits a hostile edit.
const maxNodeLabelsBytes = 16 << 10

type Controller struct {
	kubeClient client.Client
	// apiReader reads straight from the API server. The controller uses it to
	// re-check a NodeClaim and an existing Node right before and after creating a
	// Node, when a stale cache entry would cause a wrong decision.
	apiReader client.Reader
	gke       gke.Provider
	recorder  events.Recorder
	now       func() time.Time
	projectID string
	grace     time.Duration

	mu       sync.Mutex
	lastMode gke.NodeCreationMode
	// reported records, per NodeClaim, the one-time reports already made, so a
	// claim that stays stuck does not repeat them on every reconcile.
	reportedFor map[types.UID]map[string]struct{}
}

// NewController creates the controller. projectID is the project whose instances
// the provider owns.
func NewController(kubeClient client.Client, gkeProvider gke.Provider, recorder events.Recorder, projectID string) *Controller {
	return &Controller{
		kubeClient:  kubeClient,
		apiReader:   kubeClient,
		gke:         gkeProvider,
		recorder:    recorder,
		now:         time.Now,
		projectID:   projectID,
		grace:       DefaultGrace,
		reportedFor: map[types.UID]map[string]struct{}{},
	}
}

// candidate is an enrolled NodeClaim that may need a Node.
type candidate struct {
	nodeName   string
	providerID string
	enrolledAt time.Time
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *karpv1.NodeClaim) (reconcile.Result, error) {
	cand, ok := c.candidateFor(ctx, nodeClaim)
	if !ok {
		c.forget(nodeClaim.UID)
		return reconcile.Result{}, nil
	}

	nodes, err := nodeclaimutils.AllNodesForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		return reconcile.Result{}, err
	}
	for _, node := range nodes {
		// A differently named Node with this provider ID (for example after an
		// instance name was recycled) would make core fail registration with
		// MultipleNodesFound if we created a second one.
		if node.Name != cand.nodeName {
			c.reportOnce(nodeClaim.UID, "name-conflict", func() {
				nodeCreationTotal.Inc(map[string]string{"result": resultCollision})
				c.recorder.Publish(nodeNameConflictEvent(nodeClaim, fmt.Sprintf("Node %s already has provider ID %s; not creating Node %s", node.Name, cand.providerID, cand.nodeName)))
			})
			return reconcile.Result{}, nil
		}
	}
	if len(nodes) > 0 {
		return reconcile.Result{}, nil
	}

	create, requeue, err := c.shouldCreate(ctx, nodeClaim, cand)
	if err != nil || !create {
		return reconcile.Result{RequeueAfter: requeue}, err
	}
	return reconcile.Result{}, c.createNode(ctx, nodeClaim, cand)
}

// candidateFor applies the eligibility rules that need no API call: the claim
// belongs to this provider, is launched and not deleting, has not registered, its
// provider ID names an instance of the configured project, and the CSR controller
// recorded that the VM's kubelet was enrolled. The last rule keeps the invariant
// that a Node exists only after the VM authenticated, which GKE's control plane
// guarantees natively.
func (c *Controller) candidateFor(ctx context.Context, nc *karpv1.NodeClaim) (candidate, bool) {
	ref := nc.Spec.NodeClassRef
	if ref == nil || ref.Group != apis.Group || ref.Kind != "GCENodeClass" {
		return candidate{}, false
	}
	if !nc.DeletionTimestamp.IsZero() ||
		!nc.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue() ||
		nc.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
		return candidate{}, false
	}
	project, _, name, err := instance.ParseProviderID(nc.Status.ProviderID)
	if err != nil || project != c.projectID {
		return candidate{}, false
	}
	raw, ok := nc.Annotations[v1alpha1.AnnotationKubeletEnrolledAt]
	if !ok {
		return candidate{}, false
	}
	enrolledAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		c.reportOnce(nc.UID, "bad-enrollment", func() {
			log.FromContext(ctx).Error(err, "ignoring unparsable enrollment annotation", "nodeClaim", nc.Name, "annotation", v1alpha1.AnnotationKubeletEnrolledAt)
		})
		return candidate{}, false
	}
	return candidate{nodeName: name, providerID: nc.Status.ProviderID, enrolledAt: enrolledAt}, true
}

// shouldCreate applies the mode gate: the provider creates a Node only when the
// cluster uses control plane node creation, and never when the kubelet registers
// itself. When it declines it returns how long to wait before asking again, or zero
// when there is nothing to wait for.
//
// The mode comes from the cached cluster config, which can lag a mode change by
// its TTL. A claim that is still without a Node once the grace period has passed
// triggers one forced re-read, so a claim launched after a switch to
// VIA_CONTROL_PLANE does not wait for the cache to expire.
func (c *Controller) shouldCreate(ctx context.Context, nc *karpv1.NodeClaim, cand candidate) (create bool, requeue time.Duration, err error) {
	mode, err := gke.ResolveNodeCreationMode(ctx, c.gke)
	if err != nil {
		return false, 0, err
	}
	c.observeMode(ctx, mode)
	if mode == gke.NodeCreationModeControlPlane {
		return true, 0, nil
	}
	if wait := cand.enrolledAt.Add(c.grace).Sub(c.now()); wait > 0 {
		return false, wait, nil
	}
	if c.reported(nc.UID, "mode-refreshed") {
		return false, 0, nil
	}
	c.gke.InvalidateClusterConfig()
	mode, err = gke.ResolveNodeCreationMode(ctx, c.gke)
	if err != nil {
		return false, 0, err
	}
	c.reportOnce(nc.UID, "mode-refreshed", func() {})
	c.observeMode(ctx, mode)
	return mode == gke.NodeCreationModeControlPlane, 0, nil
}

func (c *Controller) createNode(ctx context.Context, nc *karpv1.NodeClaim, cand candidate) error {
	// The cached claim may be stale. A claim deleted since would leave a Node behind
	// that only the cloud controller manager cleans up.
	var fresh karpv1.NodeClaim
	if err := c.apiReader.Get(ctx, client.ObjectKeyFromObject(nc), &fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !fresh.DeletionTimestamp.IsZero() || fresh.UID != nc.UID {
		return nil
	}

	node, err := c.buildNode(ctx, &fresh, cand)
	if err != nil || node == nil {
		return err
	}

	err = c.kubeClient.Create(ctx, node)
	switch {
	case err == nil:
		nodeCreationTotal.Inc(map[string]string{"result": resultCreated})
		log.FromContext(ctx).Info("created Node for enrolled NodeClaim", "nodeClaim", nc.Name, "node", node.Name)
		c.recorder.Publish(nodeCreatedEvent(&fresh, node.Name))
		return nil
	case apierrors.IsAlreadyExists(err):
		return c.resolveExisting(ctx, &fresh, cand)
	default:
		return c.failed(fmt.Errorf("creating Node %s: %w", cand.nodeName, err))
	}
}

// buildNode renders the Node from the NodeClaim's node-labels annotation and the
// NodeClass. It returns a nil Node and no error when the claim cannot be served,
// which is reported once with an event; retrying would not change the outcome.
func (c *Controller) buildNode(ctx context.Context, nc *karpv1.NodeClaim, cand candidate) (*corev1.Node, error) {
	kubeLabels, err := nodeLabelsOf(nc)
	if err != nil {
		why := fmt.Sprintf("NodeClaim %s has no usable %s annotation (%v); cannot create its Node. A claim launched by an older version is replaced at the registration timeout", nc.Name, v1alpha1.AnnotationNodeLabels, err)
		c.reportOnce(nc.UID, "node-labels", func() {
			c.recorder.Publish(skippedEvent(nc, why))
			log.FromContext(ctx).Error(err, "cannot create Node", "nodeClaim", nc.Name)
		})
		nodeCreationTotal.Inc(map[string]string{"result": resultError})
		return nil, nil //nolint:nilerr // the annotation cannot repair itself, so retrying would only repeat the report
	}

	var nodeClass v1alpha1.GCENodeClass
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nc.Spec.NodeClassRef.Name}, &nodeClass); err != nil {
		return nil, c.failed(fmt.Errorf("getting GCENodeClass %s: %w", nc.Spec.NodeClassRef.Name, err))
	}

	node, dropped := renderNode(cand.nodeName, cand.providerID, &nodeClass, kubeLabels)
	if len(dropped) > 0 {
		c.reportOnce(nc.UID, "dropped-labels", func() {
			log.FromContext(ctx).Info("not applying node labels a kubelet could not set", "nodeClaim", nc.Name, "labels", dropped)
		})
	}
	return node, nil
}

// resolveExisting handles AlreadyExists: our own earlier create, a Node the
// kubelet or GKE created in the meantime, or a collision with an unrelated Node.
func (c *Controller) resolveExisting(ctx context.Context, nc *karpv1.NodeClaim, cand candidate) error {
	var existing corev1.Node
	if err := c.apiReader.Get(ctx, types.NamespacedName{Name: cand.nodeName}, &existing); err != nil {
		return c.failed(fmt.Errorf("getting existing Node %s: %w", cand.nodeName, err))
	}
	if existing.Spec.ProviderID == cand.providerID {
		nodeCreationTotal.Inc(map[string]string{"result": resultExists})
		return nil
	}
	nodeCreationTotal.Inc(map[string]string{"result": resultCollision})
	why := fmt.Sprintf("Node %s already exists with provider ID %q, not %s; leaving it unchanged", cand.nodeName, existing.Spec.ProviderID, cand.providerID)
	c.recorder.Publish(nodeNameConflictEvent(nc, why))
	return fmt.Errorf("%s", why)
}

func (c *Controller) failed(err error) error {
	nodeCreationTotal.Inc(map[string]string{"result": resultError})
	return err
}

// observeMode logs the resolved mode the first time and whenever it changes.
func (c *Controller) observeMode(ctx context.Context, mode gke.NodeCreationMode) {
	c.mu.Lock()
	previous := c.lastMode
	c.lastMode = mode
	c.mu.Unlock()
	if previous == mode {
		return
	}
	if previous == "" {
		log.FromContext(ctx).Info("resolved GKE node creation mode", "mode", mode)
		return
	}
	log.FromContext(ctx).Info("GKE node creation mode changed", "from", previous, "to", mode)
}

// reportOnce runs report the first time it is called for a claim and what.
func (c *Controller) reportOnce(uid types.UID, what string, report func()) {
	c.mu.Lock()
	seen := c.reportedFor[uid]
	if seen == nil {
		seen = map[string]struct{}{}
		c.reportedFor[uid] = seen
	}
	_, already := seen[what]
	seen[what] = struct{}{}
	c.mu.Unlock()
	if !already {
		report()
	}
}

// reported reports whether report ran for the claim and what.
func (c *Controller) reported(uid types.UID, what string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.reportedFor[uid][what]
	return ok
}

// forget drops the once-per-claim state of a claim that is no longer a candidate.
func (c *Controller) forget(uid types.UID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reportedFor, uid)
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	c.apiReader = m.GetAPIReader()
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclaim.nodecreation").
		For(&karpv1.NodeClaim{}).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
