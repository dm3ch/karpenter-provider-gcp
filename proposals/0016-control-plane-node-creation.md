# Proposal: Support GKE Control Plane Node Creation

- **Status**: Implemented
- **Authors**: @dm3ch
- **Created**: 2026-11-21
- **Related Issues**: [#798](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/798), [#778](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778), [#790](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/790)

---

## Summary

GKE can create `Node` objects from its control plane instead of letting the kubelet register itself (`nodeCreationConfig.nodeCreationMode = VIA_CONTROL_PLANE`). Karpenter relied on kubelet registration, so on such clusters its VMs boot, get a kubelet certificate and never get a `Node`: NodeClaims time out after 15 minutes and are replaced, VMs churn and pods stay pending.

The provider now creates the `Node` itself for launched, enrolled NodeClaims when the control plane will not. The kubelet adopts it and core Karpenter registration, initialization and termination run unchanged. `VIA_KUBELET` behavior does not change.

---

## Motivation

### Problem Statement

The [GKE release notes of 2026-09-04](https://docs.cloud.google.com/kubernetes-engine/docs/release-notes#other_changes_in_137) make control plane node creation the default for clusters created on 1.37.0-gke.2100000 and later. It has been available since 1.35.3-gke.1189000 and is still labeled Preview in the [GKE documentation](https://docs.cloud.google.com/kubernetes-engine/security/control-plane-node-creation). In this mode `gcp-controller-manager` creates the `Node` after the kubelet's client CSR is approved, and admission rejects kubelet `Node` creation.

Measured on a temporary GKE 1.37.1 cluster (2026-11-21):

| Probe                                   | `VIA_CONTROL_PLANE`                                                     | `VIA_KUBELET`                                 |
|-----------------------------------------|-------------------------------------------------------------------------|-----------------------------------------------|
| GKE-managed node                        | Node created by the control plane about 1 s after the CSR               | Node created by the kubelet                   |
| Karpenter NodeClaim                     | CSR approved, **no Node for 12+ min**; claim replaced at the 15 min TTL | Node about 55 s after claim creation, `Ready` |
| Standalone VM, no NodeClaim             | bootstrap CSR stays `Pending`; GKE does not approve non-MIG VMs         | same                                          |
| MIG signals spoofed on the Karpenter VM | no effect                                                               | n/a                                           |

Why `gcp-controller-manager` skips Karpenter VMs is not established (its source is not public). Creating the `Node` by hand makes the kubelet adopt it and the NodeClaim reach `Registered`/`Initialized`/`Ready`, provided the `Node` carries `spec.providerID`, the `karpenter.sh/unregistered:NoExecute` taint **and** `node.cloudprovider.kubernetes.io/uninitialized:NoSchedule` (otherwise the cloud controller manager never initializes it and `anetd` fails with `host IP unknown`), and the kubelet's registration labels (the kubelet does not apply `--node-labels` to an existing `Node`).

New 1.37.2 clusters created without the field still came up `VIA_KUBELET` in `asia-southeast1`, so the release-note default was not reproduced; the design must not depend on when the default flips.

### Goals

- Karpenter nodes register, initialize and become schedulable on `VIA_CONTROL_PLANE` clusters.
- No behavior change on `VIA_KUBELET` clusters: the provider never creates a Node there.
- No new privilege for the node (kubelet) identity.
- Robust to mode flips: `clusters.get` reports the old mode until an update finishes.
- The e2e environment runs the full suites on a 1.37 cluster in either mode.

### Non-Goals

- Replicating `gcp-controller-manager` beyond what registration needs.
- Autopilot, self-hosted provisioning, Windows nodes.
- Changing CSR authorization (#795, #778); this proposal only consumes its outcome.
- Creating `Node` objects in `VIA_KUBELET` mode, even as a fallback (see Alternatives).

---

## Proposal

```text
VIA_KUBELET (unchanged)
  Create VM -> kubelet CSR -> approved -> kubelet creates Node -> core registers

VIA_CONTROL_PLANE
  Create VM -> kubelet CSR -> approved -> [nodecreation controller creates Node]
            -> kubelet adopts Node -> CCM initializes -> core registers (finds Node by providerID)
```

### 1. Mode detection

`gke.ResolveNodeCreationMode` reads `Cluster.NodeCreationConfig.NodeCreationMode` from the provider's cached cluster config (no new API call or IAM). Only an exact `VIA_CONTROL_PLANE` counts as control plane. The mode is resolved at reconcile time, never at startup, and logged on first use and on change. There is no operator setting: the cluster's own mode decides, and in `VIA_KUBELET` the controller never creates a Node.

The cache TTL is 30 minutes, and `clusters.get` itself keeps reporting the old mode until a mode update finishes. So a claim that is still without a Node 5 minutes after enrollment triggers one forced re-read of the cluster config, once per claim, and the Node is created only if the cluster now reports `VIA_CONTROL_PLANE`. On a `VIA_KUBELET` cluster the claim keeps waiting for its kubelet.

### 2. Enrollment and the `nodecreation` controller

After approving a bootstrap CSR, the CSR controller records `karpenter.k8s.gcp/kubelet-enrolled-at` (RFC 3339) on the NodeClaim, once. Whatever verification #795 adds to approval therefore gates Node creation too.

The controller (`pkg/controllers/nodecreation`) acts only when **all** hold: the claim references this provider's `GCENodeClass`; it is launched, not deleting, with a `providerID` in the configured project; the enrollment annotation is present; it is not `Registered` and has **no** Node by `providerID` (a differently named Node with the same `providerID` is reported, not duplicated, since it would wedge the claim with `MultipleNodesFound`); and the mode gate passes. It re-reads the claim from the API, skips it if deleting, then `Create`s the Node:

- `AlreadyExists` with a matching `providerID`: done. A mismatch is a collision: warning event, never mutate.
- Any other error: backoff.
- It never updates, patches or deletes an existing Node.

**Why after enrollment and not at launch.** Enrollment is the only point at which the provider knows the VM booted and its kubelet holds an approved certificate. That keeps the invariant that a Node follows an authenticated kubelet, and avoids Nodes for VMs that never boot or enroll, for claims deleted during boot, or for refused CSRs, each an orphan to clean up. It also narrows the create-versus-delete race from the whole boot to the seconds after enrollment. The wait costs about one second: in the live run the Node appeared in the same second as the annotation.

### 3. Node content

- **Labels** come from the NodeClaim annotation `karpenter.k8s.gcp/node-labels`, a sorted `key=value` list that `CloudProvider.Create` writes once from the `kube-labels` item of the instance it just built. That is the set the VM boots with (what GKE echoes in `node.gke.io/last-applied-node-labels`), taken from the object already in hand: no extra API call, no `kube-env`, no change to `pkg/metadata`. `karpenter.sh/registered` and `initialized` are removed at write time, because core copies NodeClaim annotations onto the Node at registration and their absence before then is its race protection. The `kube-labels` set is used rather than `--node-labels` because it also carries labels only GKE's trusted components may set (for example `*.node-restriction.kubernetes.io/gke-secondary-boot-disk-*`).
- **Taints**: `karpenter.sh/unregistered=true:NoExecute`, GPU taints from the same launch code, and `node.cloudprovider.kubernetes.io/uninitialized=true:NoSchedule`. Core applies NodeClaim and startup taints at registration.
- `metadata.name` is the instance name; `spec.providerID` is `gce://<project>/<zone>/<name>`; annotation `karpenter.k8s.gcp/created-by: nodecreation-controller` marks the origin. No owner references or finalizers: core adds them at registration.

**Label policy.** The controller is a trusted component, so unlike a kubelet it may set `*.node-restriction.kubernetes.io` labels, but never more than the data it reads. It parses the annotation strictly (valid keys and values, at most 16 KiB) and drops `kubernetes.io` and `k8s.io` keys a kubelet may not self-assign (`node-role.kubernetes.io/*` among them), logging once per claim. The annotation is writable by anyone who can patch NodeClaims, the same boundary as the enrollment annotation, so the filter is defense in depth.

**No usable annotation** (claim launched by an older version, VM without a `kube-labels` item, or a VM adopted after an interrupted launch, which does not recover labels): one `NodeCreationSkipped` event, no Node, claim replaced at the registration timeout, exactly as before this feature. The annotation (about 1 KB) reaches every Karpenter Node in every mode because core syncs annotations; that is accepted.

### 4. Node deletion

The provider never deletes Nodes and adds no deletion code.

| Situation                                                            | Who removes the Node                                                                                                                                                             | Status                                         |
|----------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------|
| Registered, NodeClaim deleted (consolidation, expiry, drift, manual) | Core: registration adds the `karpenter.sh/termination` finalizer and owner reference; its termination flow drains, deletes the VM, removes the finalizer                         | Verified live                                  |
| NodeClaim deleted before registration                                | Core's NodeClaim finalizer deletes Nodes by `providerID`; the CCM lifecycle controller covers the create-versus-delete race                                                      | Relied upon; deletion spec exists, not yet run |
| Registered, VM deleted outside Karpenter                             | The cloud controller manager's node lifecycle controller deletes the Node (`DeletingNode: ... does not exist in the cloud provider`); core then finalizes the Node and NodeClaim | Verified live                                  |
| Healthy registered Node                                              | Nothing                                                                                                                                                                          | Stable for about 10 min in the live run        |

Live checks (2026-11-25/26, 1.37.1 `VIA_CONTROL_PLANE`, builds before the setting was removed): the Node was created and adopted and the claim reached `Ready`. Deleting the NodeClaim removed the Node and VM within 93 s with no provider involvement. Deleting the VM out of band removed the Node about 80 s later: the event `DeletingNode` came from `cloud-node-lifecycle-controller` (`... because it does not exist in the cloud provider`), and the NodeClaim was gone within the next few seconds. Karpenter then provisioned a replacement for the pending pod. No provider deletion code is needed.

### 5. RBAC, observability, version guard

- Chart `clusterrole-core.yaml` adds `create` to the existing `nodes` rule. No new IAM (`compute.instances.get` already required). The controller never uses node credentials.
- Events on the NodeClaim: `NodeCreatedByController`, `NodeCreationSkipped`, `NodeNameCollision`. Metric `karpenter_gcp_nodecreation_total{result="created|exists|collision|error"}`. Docs: [Node creation](../docs/node-creation.md), settings, troubleshooting, [CSR approval](../docs/security/csr-approval.md), `MIGRATION.md`.
- `MaxK8sVersion` is raised from 1.36.99 to 1.37.99, which removes the startup ERROR on 1.37 clusters. Karpenter provisioned and ran workloads on 1.37.1 in `VIA_KUBELET` mode.

### 6. E2E and infrastructure

- **Terraform** (`deploy/terraform`, `deploy/terraform-e2e`): optional `min_master_version` (a floor, validated) and `node_creation_mode` inputs; `google` provider 6.28.0 to 7.46.2 (the field needs 7.36.0 or later). The shared e2e config uses `REGULAR` (STABLE does not offer 1.37 yet), `min_master_version = "1.37"` and pins `VIA_CONTROL_PLANE`, so a flip of GKE's default cannot silently change the environment. Re-run the read-only `terraform plan` before any apply; applying is a separate live operation.
- **Specs** (`test/suites/provisioning/node_creation.go`, `suite:provisioning`, run by `standard`): one registration spec per mode, each skipping unless the cluster's reported mode matches (it fails instead of skipping while a cluster operation runs, because the mode is stale then), and a mode-independent deletion-before-registration spec. A registration spec asserts: the claim's Node is Ready within 5 minutes of claim creation with no replacement claim; `registered`/`initialized`; GKE readiness labels match a MIG node of the same cluster; every kube-system DaemonSet meant for the node has a Ready pod; and the `created-by` annotation is present in `VIA_CONTROL_PLANE` and absent in `VIA_KUBELET`. The deletion spec checks the claim is still unregistered before deleting its pool and leaves no Node or VM.
- On a timeout the spec dumps NodeClaim conditions, CSRs and their approver, Node events and the cluster mode. The runner report gains an `Environment` line (Kubernetes version, node creation mode). The channel image spec skips when a channel has no version for the cluster's minor.
- **Validation**: `make e2e-deploy` and `make e2e-tests E2E_SELECTION=standard GINKGO_PROCS=20` from the branch under test; results are reported on #803. Re-run in `VIA_CONTROL_PLANE` for releases and for PRs touching registration, CSR approval, bootstrap metadata or instance creation. Continuous coverage of both modes needs a second environment or switching one cluster between runs, which is not designed here. A scheduled canary on a throwaway cluster without the mode set would be the only way to observe the day the default flips; release-note review (`gke-release-notes-review`) is the cheap alternative.

---

## Risks and Mitigations

| Risk                                                                                              | Likelihood    | Impact                       | Mitigation                                                                                                                                                                         |
|---------------------------------------------------------------------------------------------------|---------------|------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Adoption of a pre-existing Node rests on one observation on 1.37.1, not a documented GKE contract | Low to medium | High (feature stops working) | Unsupported by Google by definition (they document no way to bring your own VMs into GKE), so documented as such; e2e on every supported minor; events and metric expose failure   |
| Created Node is incomplete versus a GCM-created one                                               | Medium        | Medium                       | Labels from the launched VM's `kube-labels`; e2e diffs against a MIG node and requires every GKE DaemonSet Ready                                                                   |
| Mode misreported during a flip (cached or stale `clusters.get`)                                   | Medium        | Low                          | One forced config re-read per overdue claim; the claim is replaced at the registration timeout if that is not enough                                                               |
| Privilege increase (`nodes: create`) and label trust (bypasses `NodeRestriction`)                 | Low           | Medium                       | Create only for enrolled, launched, provider-owned claims; kubelet-equivalent label filter; annotation has the same trust boundary as the enrollment annotation; negative e2e test |
| Orphan or duplicate Node (claim deleted mid-creation, recycled name, same `providerID`)           | Low           | Medium                       | Re-read claim before create; `providerID` check before create; no finalizer until core registers; CCM and core GC as safety net; tests for each case                               |
| Overlap with CSR attestation (#795)                                                               | Medium        | Low                          | The CSR controller owns the enrollment annotation; this feature never re-implements CSR authorization                                                                              |

---

## Test Plan

### Unit Tests

- `NodeCreationModeOf`: nil config, both modes, unknown value, API error, cache and invalidation.
- `Create` writes `node-labels` from the launched VM's `kube-labels`, without `registered`/`initialized`, and writes nothing when the item is absent.
- Controller eligibility: non-provider claim, not launched, deleting, registered, no enrollment, Node exists by `providerID`, differently named Node with the same `providerID`, mode gate (never creates in `VIA_KUBELET`), grace boundary from enrollment, one forced mode re-read per claim and a retry after a failed re-read.
- Annotation handling: missing, malformed, invalid label, oversized (each reported once per claim). Rendered Node: both taints, GPU taints, `registered` label absent, secondary-boot-disk label kept, unsafe keys dropped, `AlreadyExists` with matching and mismatching `providerID`, no finalizer or owner reference.
- The CSR controller writes the enrollment annotation only on a successful approval of a launched, unregistered, provider-owned claim.

### E2E

- `VIA_CONTROL_PLANE`: launch, registration, GKE readiness labels, label diff against a MIG node, DaemonSets, termination, no leaked Nodes; GPU, local-SSD and secondary-boot-disk labels.
- Claim deleted before registration leaves no Node or VM. Registered node with a VM deleted out of band: Node and NodeClaim removed (verified manually; not yet a spec).
- No Node for a non-provider or unenrolled claim.
- `VIA_KUBELET` regression: unchanged timing and no controller-created Nodes. A `KUBELET` to `CONTROL_PLANE` flip mid-run exercises the forced re-read.

---

## Acceptance Criteria

- [X] A NodePool on a `VIA_CONTROL_PLANE` cluster provisions nodes that become Ready (verified manually on 1.37.1; full suite run reported on #803).
- [ ] `VIA_KUBELET` timings are unchanged and covered by the existing suites.
- [X] No Node is created on a `VIA_KUBELET` cluster.
- [X] Events, metric and logs identify a stuck registration and its cause.
- [X] Docs cover both modes, troubleshooting, the `nodes: create` privilege and that adoption is not a documented GKE contract.
- [ ] e2e runs the suites on a 1.37 cluster in both modes and on every supported minor.

---

## Migration

No action on `VIA_KUBELET` clusters. On `VIA_CONTROL_PLANE` clusters, upgrade the chart, which includes `nodes: create`; operators who manage the ClusterRole themselves must add it. NodeClaims launched before the upgrade have no `node-labels` annotation and are replaced at the registration timeout.

---

## Alternatives Considered

- **Create the Node in every mode.** Removes mode detection and flip races and exercises one path, but changes registration on every existing cluster, and any label the provider fails to reproduce would regress healthy clusters. Revisit once data exists.
- **Create the Node inside `CloudProvider.Create`, or when the claim is created or launched.** Couples VM launch to Kubernetes writes before the kubelet authenticated, creates Nodes for VMs that never boot, and widens the create-versus-delete race to the whole boot. A separate controller is idempotent, retryable and testable.
- **An operator setting (`auto`/`kubelet`) or a create-anyway fallback on `VIA_KUBELET`.** An earlier revision had both. The kill switch protects nothing that matters (without this controller nothing registers on `VIA_CONTROL_PLANE`, and a rejected `Create` only backs off), and creating a Node under `VIA_KUBELET` is untested and changes registration there. The forced re-read covers the stale-mode window instead.
- **Core `NodeLifecycleHook`.** It only gates when registration completes; it runs after the Node exists and cannot create it.
- **Other label sources.** Recomputing from the NodeClass and NodeClaim can diverge from the launched VM (inherited readiness labels vary by source pool). Reading `kube-labels` back from the VM when creating the Node needs a Compute read and parser in the controller and trusts `compute.instances.setMetadata` writers. Parsing `KUBELET_ARGS` from `kube-env` is secret-bearing and omits `kube-labels`-only labels. `NodeClaim.metadata.labels` would make core copy about 20 GKE-internal labels onto every Node in every mode, including `VIA_KUBELET` clusters, and `registered` would reach the Node before registration finishes; it fits the "create in every mode" alternative, where that is intended.
- **Wait for Google to support non-MIG VMs.** Unknown timeline; users on default-CP clusters need a working path now.
- **Block provisioning on `VIA_CONTROL_PLANE`.** Cheap, but leaves the default mode of new clusters unsupported.

---

## Future Direction

If GKE deprecates `VIA_KUBELET`, the mode check and the forced re-read go away and the controller always creates the Node. We do not expect Google to support standalone VMs natively; if it ever does, the controller finds the Node already present and does nothing, and can be removed.

---

## Questions Settled

- **Supported path from Google.** None, and none is expected: Google documents no way to bring your own VMs into GKE, so adoption of a pre-existing Node is observed behavior, not a contract. It is documented as unsupported and guarded by the e2e specs.
- **Label reconciliation.** Settled by the 2026-11-25 live run: `gcp-controller-manager` is among the field managers of the adopted Node, which carries `node.gke.io/last-applied-node-labels` with the same set the provider applied, and the Node's labels matched it. GKE and the provider apply the same values; the e2e label diff against a MIG node stays as the regression guard.
- **Grace period.** 5 minutes before the forced mode re-read. The kubelet registers about 1 s after its certificate and a Node is Ready within about a minute of launch, so a healthy claim never reaches it; core replaces a claim that has not registered 15 minutes after creation and enrollment takes about 1.5 minutes, so the re-read still leaves about 8 minutes to create the Node and register. A longer delay would risk the registration timeout, a shorter one buys nothing because a mode update takes 4 to 15 minutes anyway.
- **Reported default.** Not needed for the design: new 1.37.2 clusters were `VIA_KUBELET` despite the release note, and the controller follows whatever the cluster reports.
- **Out-of-band VM deletion.** Settled by the 2026-11-26 live run: the cloud controller manager's node lifecycle controller removes the Node about 80 s after the VM disappears, and core removes the NodeClaim; see section 4.
- **Certificate rotation.** Not testable in practice: the kubelet client and serving certificates issued to Karpenter nodes are valid for five years (`notAfter` 2031 on a 2026 issue), and the kubelet renews at 70-90% of that lifetime, far beyond any node's life under consolidation, expiry or drift. The renewal path (`system:node:<name>` self-renewal of a registered claim) is covered by the CSR controller's unit tests and is unchanged by this work.
