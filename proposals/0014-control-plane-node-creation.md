# Proposal: Support GKE Control Plane Node Creation

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-11-21
- **Related Issues**: [#798](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/798), [#778](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778), [#790](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/790)

---

## Summary

GKE can create `Node` objects from the control plane instead of letting the kubelet self-register (`NodeCreationConfig.nodeCreationMode = VIA_CONTROL_PLANE`). Karpenter GCP relies on kubelet self-registration, so on such clusters Karpenter-launched VMs boot, obtain a kubelet client certificate, and never get a `Node`. NodeClaims time out after 15 minutes and are replaced, so billed VMs churn while pods stay pending, with no error pointing at the cause.

This proposal makes the provider work in both modes. The provider detects the mode from the GKE cluster read it already performs and, when the control plane will not create the `Node`, creates it itself for provider-owned, already-enrolled NodeClaims. The kubelet then adopts the existing `Node` and the core Karpenter lifecycle (registration, initialization, termination) runs unchanged. Existing `VIA_KUBELET` behavior does not change.

---

## Motivation

### Problem Statement

GKE release notes (2026-09-04) state that clusters created on 1.37.0-gke.2100000 and later default to control plane node creation; the mode has been available since 1.35.3-gke.1189000. In this mode `gcp-controller-manager` creates the `Node` after the kubelet's client CSR is approved, and an admission controller rejects kubelet `Node` creation.

Evidence from a temporary cluster (GKE 1.37.1-gke.2097000, RAPID channel, Shielded nodes, Dataplane V2), 2026-11-21:

| Probe                                                                            | `VIA_CONTROL_PLANE`                                                                                                                        | `VIA_KUBELET`                                                                        |
|----------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------|
| GKE-managed (MIG) node                                                           | Node created by the control plane about 1 s after the CSR                                                                                  | Node created by the kubelet                                                          |
| Karpenter NodeClaim                                                              | VM boots, kubelet runs, Karpenter approves the CSR, **no Node for 12+ min**; NodeClaim deleted at the 15-min registration TTL and replaced | Node appears about 55 s after NodeClaim creation, `Registered`/`Initialized`/`Ready` |
| Standalone VM from the pool template, no NodeClaim                               | bootstrap CSR stays `Pending`; GKE does not approve non-MIG VMs                                                                            | same                                                                                 |
| MIG signals spoofed on the Karpenter VM (`created-by` metadata, node-pool label) | no effect                                                                                                                                  | n/a                                                                                  |

Why `gcp-controller-manager` skips Karpenter VMs is **not established**: its source is not public and audit logs were unavailable. The working hypothesis is that node creation is tied to GKE's own attested CSR approval, which standalone VMs fail.

A manual proof of concept shows the provider can compensate. Creating the `Node` by hand makes the kubelet adopt it; the NodeClaim reached `Registered`/`Initialized`/`Ready`, a test pod ran, and every GKE DaemonSet was Running, provided the `Node` carried:

- `spec.providerID`, the `karpenter.sh/unregistered=true:NoExecute` taint, **and** `node.cloudprovider.kubernetes.io/uninitialized=true:NoSchedule`. Without the second taint the cloud controller manager never initializes the node (no `InternalIP`) and `anetd` fails with `host IP unknown`.
- The kubelet `--node-labels` set from the VM's `kube-env`. The kubelet does not apply `--node-labels` to a pre-existing `Node`; without them GKE readiness labels are missing and DaemonSets do not schedule.

The e2e cluster, new 1.37 clusters, and existing clusters that opt in are therefore split across a registration path the provider has never supported. On 1.37 clusters the provider also logs an ERROR at startup (`MaxK8sVersion = 1.36.99`), which this work revisits.

Observed contract gap: new 1.37.2-gke.1606000 clusters created via gcloud and via the REST API without the field came up `VIA_KUBELET` in `asia-southeast1`, so the release-note default was not reproduced. The design must not depend on when the default flips.

### Goals

- Karpenter-launched nodes register, initialize and become schedulable on `VIA_CONTROL_PLANE` clusters.
- No behavior change on `VIA_KUBELET` clusters.
- Fail loudly and early, with an actionable message, whenever the provider cannot register nodes.
- Do not grant the node (kubelet) identity any new privilege; the control plane keeps sole authority over `Node` creation as far as nodes are concerned.
- Keep the design robust to mode flips (an update takes minutes and `clusters.get` reports the old mode until it finishes).
- Make the e2e environment able to run the full suites on a 1.37 cluster in either mode.

### Non-Goals

- Reproducing or replacing `gcp-controller-manager` logic beyond what registration needs.
- Autopilot, non-GKE (self-hosted) provisioning, Windows nodes.
- Changing CSR authorization policy; that is the scope of proposal 0012 / #778. This proposal only consumes its outcome.
- Making the provider create `Node` objects in `VIA_KUBELET` mode by default (see Alternatives).

---

## Proposal

### Overview

```text
                                  VIA_KUBELET (today, unchanged)
NodeClaim -> Create VM -> kubelet CSR -> Karpenter approves -> kubelet creates Node -> core registers

                                  VIA_CONTROL_PLANE (this proposal)
NodeClaim -> Create VM -> kubelet CSR -> Karpenter approves -> [nodecreation controller creates Node]
                                                              -> kubelet adopts Node -> CCM initializes
                                                              -> core registers (finds Node by providerID)
```

A new controller, `nodecreation`, reconciles provider-owned NodeClaims. When a claim is launched, enrolled and has no `Node`, and the cluster is in a mode in which GKE will not create it, the controller creates the `Node` from the registration data the provider already wrote into the VM metadata, then steps aside.

### Design Details

#### 1. Mode detection (`pkg/providers/gke`)

`pkg/providers/gke` already owns shared cluster facts and caches `GetClusterConfig`. Add `NodeCreationMode(ctx) (NodeCreationMode, error)` reading `Cluster.NodeCreationConfig.NodeCreationMode`; nil, empty or unknown-but-not-control-plane maps to `Kubelet`. Treat only an exact `VIA_CONTROL_PLANE` as control plane. No new API call and no new IAM.

Log the resolved mode once at startup, and again on change, with the `Registered=Unknown NodeNotFound` troubleshooting hint. Callers must not cache the result themselves; use the provider cache and `InvalidateClusterConfig`.

#### 2. Operator setting `nodeCreation`

New setting (`NODE_CREATION` env / `controller.settings.nodeCreation` in the chart):

| Value            | Behavior                                                                                                                                                                                                                                                         |
|------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `auto` (default) | Create the Node for an enrolled claim as soon as the cluster reports `VIA_CONTROL_PLANE`; otherwise only if the claim is still without a Node a grace period (default 45 s, measured from enrollment, see section 3) after the cluster configuration was re-read |
| `kubelet`        | Never create Nodes; current behavior and the escape hatch                                                                                                                                                                                                        |

The reported mode lags a flip: during a `KUBELET` to `CONTROL_PLANE` update `clusters.get` still returns the old value while the new behavior already applies. So when a claim is stuck, the controller first forces `InvalidateClusterConfig` and re-reads the mode; if the cluster still reports `VIA_KUBELET` it creates the Node anyway once the grace period elapsed and emits a `NodeCreationModeMismatch` warning event. In observed `VIA_KUBELET` runs the kubelet registered about 1 s after its certificate was issued, so 45 s from enrollment should not race a healthy kubelet. Adoption of an existing Node is the same kubelet code path in both modes, but creating a Node early in `VIA_KUBELET` mode is **untested**; the e2e plan includes it.

#### 3. Enrollment evidence and the controller `pkg/controllers/nodecreation`

**Enrollment record.** The CSR controller (`pkg/controllers/csr`) already decides whether a kubelet client CSR belongs to a launched, unregistered, provider-owned NodeClaim. After a successful approval it also records the fact on that NodeClaim (annotation `karpenter.k8s.gcp/kubelet-enrolled-at: <RFC3339>`, written once with `NodeClaim` patch rights the controller already has). The new controller consumes this record instead of re-parsing CSRs or matching the approval reason string, and it does not depend on CSR objects surviving garbage collection. The CSR controller is also changed by proposal 0012 (#795); ownership of that code stays with the CSR controller, so whatever verification 0012 adds gates the annotation automatically.

**Controller.** Follows the pattern of the CSR controller (operator cached client, event-driven on NodeClaim and Node, `RequeueAfter` for the grace period). For each NodeClaim, act only when **all** hold:

1. `spec.nodeClassRef` is a `GCENodeClass` of this provider.
2. `Launched=True`, `DeletionTimestamp` is zero, `status.providerID` parses to `gce://<project>/<zone>/<name>` for the configured project.
3. The enrollment annotation is present. This preserves the invariant that a Node exists only after the VM's kubelet authenticated, which `gcp-controller-manager` guarantees natively.
4. `Registered` is not `True` and **no Node** matches the claim by `providerID`, using core's `NodeForNodeClaim` helper. A differently named Node with the same `providerID` (for example after an instance name was recycled) would wedge the claim with `Registered=False/MultipleNodesFound`, so it is detected here and reported with an event instead of creating a second Node.
5. The mode gate from section 2 passes.

Action: re-read the NodeClaim from the API (not the cache) and skip if it is now deleting, then `Create` the Node (section 4). Results:

- `AlreadyExists`: `Get` it. If `spec.providerID` matches, succeed. If not, emit a warning event, return an error and never mutate it; a different Node owning the name is a collision, not ours to repair.
- Any other error: return for backoff; there is no partial state.
- Never `Update`/`Patch` an existing Node. After creation the kubelet, CCM and core own it.

#### 4. Node content

The `Node` must equal what the kubelet would have registered and must not require the controller to read secret-bearing metadata.

- **Labels** come from the VM's non-secret `kube-labels` metadata item, the full label set the provider assigns to the node (what GKE echoes in `node.gke.io/last-applied-node-labels`). Extract only that item from the instance read inside `pkg/providers/instance` and pass the parsed map in; `pkg/metadata` gains a typed reader and stays free of API calls per `.greptile/rules.md`. Never log or cache the whole metadata response, which also carries `kube-env`. `karpenter.sh/registered` is present in `kube-labels` and **must be removed**: core adds it when registration completes, and its absence before that is the signal core's race protection relies on.
- **Taints** are recomputed with the same functions used at launch: the fixed `karpenter.sh/unregistered=true:NoExecute`, GPU taints (`gpuInstanceMetadata`) derived from the NodeClaim and its NodeClass, plus `node.cloudprovider.kubernetes.io/uninitialized=true:NoSchedule`. NodeClaim taints and startup taints are applied by core at registration. Launch-time and creation-time NodeClass drift between the two is the same exposure as for any NodeClass edit and is surfaced by existing drift detection.
- `metadata.name`: instance name (matches the kubelet's `CN=system:node:<name>`).
- `spec.providerID`: `gce://<project>/<zone>/<name>`.
- `metadata.annotations`: `karpenter.k8s.gcp/created-by: nodecreation-controller` for diagnostics; neither core registration nor the kubelet removes unknown annotations.
- No owner references or finalizers. Core registration adds them. Until then the Node has no core termination finalizer, so cleanup of a claim deleted before registration depends on core's NodeClaim finalizer (deletes Nodes by `providerID`) and, for the narrow create-versus-delete race that a re-read cannot make atomic, on the cloud controller manager's lifecycle controller removing Nodes whose instance is gone. This is relied upon and tested, not assumed.

**Why `kube-labels` and not `--node-labels`.** The provider maintains both sets and they deliberately differ: `kube-labels` also carries labels GKE applies through a trusted component and that a kubelet is not allowed to set, such as `cloud.google.com.node-restriction.kubernetes.io/gke-secondary-boot-disk-*` (written only via `SetKubeLabel`) and `karpenter.sh/registered`. `--node-labels`, the kubelet's own registration set, is therefore not a complete picture. Using `kube-labels` also means no secret-bearing `kube-env` parsing.

**Label policy.** The controller is a trusted control plane component like `gcp-controller-manager`, so it may set `*.node-restriction.kubernetes.io` labels that a kubelet cannot. It must still never be more permissive than the data's trust boundary: keys come only from the provider-written metadata item. Keys in the `kubernetes.io` and `k8s.io` namespaces other than those a kubelet may self-assign (`kubernetes.io/{hostname,arch,os}`, `node.kubernetes.io/*`, `topology.kubernetes.io/{region,zone}`, legacy `failure-domain.beta.kubernetes.io` and `beta.kubernetes.io` equivalents, `kubelet.kubernetes.io/*`) are dropped and logged once per claim; `node-role.kubernetes.io/*` is among the dropped. VM metadata is writable by anyone with `compute.instances.setMetadata`, the same boundary that feeds the kubelet today, so the filter is defense in depth, not a new boundary. Whether GKE also reconciles `kube-labels` onto an adopted Node is unknown (open question 2); the controller setting them is harmless either way because it applies the same values.

#### 5. RBAC and IAM

- Chart `clusterrole-core.yaml`: add `create` to the existing `nodes` write rule (today `delete`, `update`, `patch`). No other Kubernetes permission is new. `deploy/iam/karpenter-controller-role.yaml` needs no change: `compute.instances.get` is already required.
- The controller never uses node credentials; node identity gains nothing, preserving the point of control plane node creation.

#### 6. Observability

- Events on the NodeClaim: `NodeCreatedByController`, `NodeCreationSkipped` (with reason), `NodeCreationModeMismatch`, `NodeNameCollision`.
- Metric `karpenter_gcp_nodecreation_total{result="created|exists|collision|error"}`.
- Startup and mode-change log lines; a clear error log, once per claim, when a claim is launched, enrolled, unregistered past the grace period and the setting is `kubelet`.
- Docs: supported modes and GKE versions, the `nodeCreation` setting, troubleshooting (`Registered=Unknown NodeNotFound`, kubelet `NodeRegistrationCheckerStart` events, CSR approved but no Node).

#### 7. Kubernetes version guard

Revisit `MaxK8sVersion` (currently `1.36.99`, logging an ERROR on 1.37) once the e2e suites pass on 1.37.

#### 8. E2E and infrastructure prerequisites

Delivered with this proposal so the fix can be validated on a 1.37 cluster:

- `deploy/terraform` gains `min_master_version` (optional, validated, passed to `google_container_cluster.min_master_version`). It is a floor; GKE keeps auto-upgrading inside the release channel, which must offer the version. The e2e root passes it through and the shared `terraform.tfvars` sets `release_channel = "REGULAR"` and `min_master_version = "1.37"` because `STABLE` does not yet offer 1.37. The module also gains `node_creation_mode` (`VIA_KUBELET` or `VIA_CONTROL_PLANE`, rendered as `node_creation_config`), the `google` provider moves from 6.28.0 to 7.46.2 (the field needs 7.36.0 or later), and the shared e2e config pins `VIA_KUBELET` explicitly. A read-only `terraform plan` (no lock, nothing applied) against the shared state shows exactly two in-place cluster changes and nothing else: `min_master_version` set to `1.37` and `release_channel` `STABLE` to `REGULAR`, so the provider bump itself causes no drift and the pinned mode matches the current one. Applying it is a separate, explicitly approved live operation that upgrades the control plane. A `control-plane` profile (`karpenter-e2e-cp`, own state) is committed for the `VIA_CONTROL_PLANE` runs; see section 9.
- The channel image-selection spec skips itself when the selected channel offers no version for the cluster's Kubernetes minor. `ResolveVersionForChannel` exposes `ErrNoVersionForClusterMinor`; the e2e helper `SkipIfChannelLacksClusterMinor` skips only on that error, so other resolution failures still fail the spec.

The control-plane enrolment profile and its specs are designed in section 9.

#### 9. E2E validation design

**Goal.** Prove at least once, and then keep proving, that the provider provisions working nodes when the cluster uses control plane node creation, so a GA or default flip by GKE cannot silently break users. Today the persistent e2e cluster is `VIA_KUBELET` and every suite runs only there.

**Principles.**

- Pin the mode explicitly in every e2e profile. A profile that relies on the GKE default changes behavior silently the day the default flips, including when a cluster is recreated.
- Use the existing tooling unchanged where possible. `make e2e-deploy`, `e2e-tests`, `e2e-clean-env` and the runner already derive every name (cluster, pod ranges, tag key, registry, service accounts) from `E2E_PREFIX`, so a second environment is a second prefix, not new scripts.
- Show red before green: the profile is built first and run against current `main` to demonstrate that the specs detect the bug, then against the fix.

**Profiles.**

| Profile                                | Prefix             | Mode                        | State                                                |
|----------------------------------------|--------------------|-----------------------------|------------------------------------------------------|
| shared (existing cluster)              | `karpenter-e2e`    | `VIA_KUBELET`, pinned       | persistent                                           |
| `control-plane` (committed in this PR) | `karpenter-e2e-cp` | `VIA_CONTROL_PLANE`, pinned | created per validation (tier 1), or persistent later |

The shared cluster stays `VIA_KUBELET` until the provider supports the other mode, so unrelated branches keep a green environment while the fix is built and validated. `profiles/control-plane.tfvars` and its backend config (own state prefix) are committed; the e2e Terraform prefix guard allows exactly `karpenter-e2e` and `karpenter-e2e-cp` in the sponsored project. A read-only plan of the profile creates 18 resources and changes nothing existing. Once the fix is validated, the shared pin can move to `VIA_CONTROL_PLANE` and `VIA_KUBELET` regression coverage can take over the profile slot, with the same inputs.

Both use the same region, zones, Kubernetes minor and topology so suite results are comparable; they must not run tests at the same time because their combined CPU/IP quota at 20 workers is not sized for it (sequential runs, or a CI matrix with `max-parallel: 1`).

**Infrastructure changes (`deploy/terraform`, `deploy/terraform-e2e`).**

1. Done in this PR: `node_creation_mode` input on the cluster module and the e2e root, `google` provider 7.46.2 (the field needs 7.36.0 or later), shared config pinned to `VIA_KUBELET`, `profiles/control-plane.*` committed, guard allow-list, verified by the read-only plans described in section 8 and above.
2. Profile files under `deploy/terraform-e2e/profiles/` (non-secret variables plus a backend config with its own state prefix, `karpenter/e2e-cp`). The `prefix` guard in `variables.tf` changes from "only `karpenter-e2e`" to an explicit allow-list of the known profile prefixes, so an arbitrary prefix is still rejected in the sponsored project. A distinct `controller_role_id` (custom role IDs are project-scoped) and non-overlapping `additional_pod_ranges` names follow from the prefix.
3. README: how to create, use and retire a profile, the cost (a second regional control plane and one system node), and the rule that applying or destroying a profile is a separate live operation needing approval.

**Tier 1: validate once.**

1. Apply the pinned Terraform config (the `control-plane` profile). The cluster operation takes 4 to 15 minutes and `clusters.get` keeps reporting the old mode until it ends; wait for `RUNNING` plus `VIA_CONTROL_PLANE`, and check the mode again after the run.
2. `make e2e-deploy` and `make e2e-tests E2E_SELECTION=standard GINKGO_PROCS=20` with the profile's `E2E_PREFIX`, from the branch under test. The runner lease is per cluster.
3. For a temporary profile, retire it (Terraform destroy) when done.

This is what was done by hand, with a `gcloud` flip, for the evidence in this proposal; tier 1 makes it repeatable and records it with a report.

**Tier 2: both profiles in CI.** The on-demand e2e pipeline (proposal 0005, #708) takes the profile (prefix) as an input, defaulting to the shared cluster, and releases run both. Because two clusters share the project, `e2e-check-clean.sh` must filter by the cluster label `goog-k8s-cluster-name` instead of the name prefix `karpenter-` so one profile's check does not report the other's instances.

**Tier 3 (optional): default-flip canary.** A scheduled job creates a throwaway cluster **without** specifying the mode on the newest RAPID version, records the mode GKE chose, runs the provisioning smoke spec below, and tears the cluster down. It is the only check that observes what the default actually is on the day it changes, independent of any pinned profile. Release-note review (`gke-release-notes-review`) is the cheap alternative.

**Test changes.**

- `Environment.NodeCreationMode(ctx)` in `test/pkg/environment`, from the GKE cluster read the environment already has access to; it is logged at suite start and used by specs to pick assertions.
- New specs in `test/suites/provisioning/node_creation.go` (committed with this proposal, labelled `suite:provisioning` so `standard` runs them). One registration spec per mode, each skipping itself unless the cluster's reported mode matches (`Environment.SkipUnlessNodeCreationMode`; it fails instead of skipping while a cluster operation is running, because the reported mode is stale then), plus a mode-independent deletion spec. The registration specs share this body:
  1. Provision a node and assert Ready within a bound that detects churn (for example 5 minutes, well below the 15-minute registration TTL), `karpenter.sh/registered` and `initialized`, the NodeClaim `Registered` condition.
  2. Assert GKE readiness labels: every key from a fixed list of GKE readiness labels (for example `addon.gke.io/node-local-dns-ds-ready`, `iam.gke.io/gke-metadata-server-enabled`, `cloud.google.com/gke-netd-ready`) that exists on a GKE-managed node of the same cluster also exists on the Karpenter node. This is the label diff against a MIG node required by the Test Plan.
  3. Assert GKE system DaemonSets are Ready on the node (`netd`/`anetd`, `gke-metadata-server`, `gke-metrics-agent`, `pdcsi-node`), the check that caught the missing `uninitialized` taint.
  4. Mode-specific: in `VIA_CONTROL_PLANE`, assert the Node carries `karpenter.k8s.gcp/created-by`; in `VIA_KUBELET`, assert the annotation is absent. On current `main` the control-plane spec is the expected red result and the kubelet spec passes.
  5. Delete the NodePool before the node is Ready and assert no Node or VM is left (the creation-versus-delete race).
- On a registration timeout the spec dumps a diagnostic bundle instead of a bare timeout: NodeClaim conditions, CSRs for the instance (state and approver), Node events, `NodeRegistrationChecker` events, and the cluster's mode. This is exactly the evidence that took manual work to collect here.
- Runner report (`hack/e2e-runner`) gains an `Environment` line with Kubernetes version and node creation mode, read from the GKE API using the `PROJECT_ID`, `CLUSTER_NAME` and `CLUSTER_LOCATION` the runner already receives, so a report can never be mistaken for the wrong profile.
- Existing suites run unchanged in both profiles. No spec deletes Node objects or restarts kubelets today (checked by search), so no existing spec is expected to depend on kubelet self-registration; the first control-plane run confirms it.

**Run order and acceptance.**

1. Tier 1 against current `main`: expect the new spec to fail with the diagnostic bundle (red), and the rest of the suites to fail at registration. This proves the harness detects the issue.
2. Tier 1 against the Phase 3 branch: the full `standard` selection passes on both profiles (green), with the report showing each profile's mode and Kubernetes minor.
3. Keep the control-plane profile for releases and for any PR that touches registration, CSR approval, bootstrap metadata or instance creation.

**Cost and limits.** A second regional control plane plus one `n2-standard-2` system node (order of magnitude `$100` per month); tier 1 pays only for the validation window. Running both profiles doubles the quota need only if they run concurrently, which this design forbids. GPU and extended local SSD stay outside `standard` as today.

## Risks and Mitigations

| Risk                                                                                                                                                                                                                     | Likelihood    | Impact                       | Mitigation                                                                                                                                                                                                                                 |
|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| The mechanism rests on one observation (kubelet `Create` on an existing Node reaches adoption on 1.37.1) and is not a documented GKE contract; a stricter admission plugin could reject before the `AlreadyExists` check | Low to medium | High (feature stops working) | Say so in the docs; acceptance requires the e2e spec to pass on every GKE minor claimed as supported; raise it with Google (open question 1); events and metric make failure visible; `kubelet` setting and Phase 1 diagnostics still work |
| Created Node is incomplete versus a GCM-created Node (labels/annotations GKE expects)                                                                                                                                    | Medium        | Medium                       | Labels read back from the VM's `kube-labels`; e2e diffs labels against a MIG node in the same cluster and asserts every GKE DaemonSet becomes Ready                                                                                        |
| Mode misreported during a flip                                                                                                                                                                                           | Medium        | Low                          | Forced cluster-config refresh plus grace-period fallback, mismatch event                                                                                                                                                                   |
| Early creation in `VIA_KUBELET` mode races a healthy kubelet                                                                                                                                                             | Low           | Low (adoption path)          | Grace measured from enrollment (kubelet registered ~1 s after cert in observed runs); explicit e2e case                                                                                                                                    |
| Controller privilege increase (`nodes: create`)                                                                                                                                                                          | Low           | Medium                       | Narrow create path (enrolled, launched, provider-owned claims only), negative e2e test, security docs                                                                                                                                      |
| Label trust: controller-created Node bypasses `NodeRestriction`                                                                                                                                                          | Low           | Medium                       | Keys only from the provider-written `kube-labels` item, kubelet-equivalent filter for `kubernetes.io`/`k8s.io` keys, same metadata trust boundary as today                                                                                 |
| Orphan or duplicate Node (claim deleted mid-creation, recycled name, stale Node with the same providerID)                                                                                                                | Low           | Medium                       | Re-read claim before create, `NodeForNodeClaim` providerID check before create, no finalizers or owner refs until core registers, CCM lifecycle controller as safety net, tests for each case                                              |
| Overlap with proposal 0012 CSR changes                                                                                                                                                                                   | Medium        | Low                          | The CSR controller owns and writes the enrollment annotation; this proposal never re-implements CSR authorization                                                                                                                          |

---

## Test Plan

### Unit Tests

- `NodeCreationMode`: nil config, `VIA_KUBELET`, `VIA_CONTROL_PLANE`, unknown value, API error, cache and invalidation.
- `pkg/metadata` typed `kube-labels` reader: round-trip against the setters; malformed input; the entire metadata response is never retained.
- Which keys may differ between `kube-labels` and `--node-labels` (secondary-boot-disk `*.node-restriction.kubernetes.io` label, `karpenter.sh/registered`), asserted explicitly.
- Enrollment annotation written by the CSR controller only on a successful approval for a launched, unregistered, provider-owned claim.
- Controller eligibility table: non-provider claim, not launched, deleting, registered, no enrollment annotation, Node exists by providerID, a differently named Node with the same providerID, mode gate, `nodeCreation` values, grace boundaries measured from enrollment, forced refresh.
- Rendered Node: `uninitialized` and `unregistered` taints present, GPU taints, `registered` label absent, secondary-boot-disk label kept, `node-role.kubernetes.io/*` and other unsafe keys dropped, providerID format, `AlreadyExists` with matching and mismatching providerID, no finalizer or owner reference.
- `ResolveVersionForChannel` error matches `ErrNoVersionForClusterMinor` and nothing else does.

### E2E / Integration Tests

- Control-plane profile: launch, registration, `karpenter.sh/registered`/`initialized`, GKE readiness labels, GPU, local-SSD and secondary-boot-disk labels, label diff against a MIG node, CSR approval and certificate rotation, termination, no leaked Nodes.
- Claim deleted before registration: Node removed (core finalizer path and CCM safety net), no orphan.
- Negative: no Node is created for a non-provider claim or an unenrolled claim.
- Kubelet profile regression: unchanged timing and no controller-created Nodes (annotation absent); a forced early-creation case.
- Flip test (manual or nightly): `KUBELET` to `CONTROL_PLANE` mid-run exercises the fallback.
- The spec runs on every GKE minor the docs claim to support.

---

## Acceptance Criteria

The feature is complete when:

- [ ] A NodePool on a `VIA_CONTROL_PLANE` cluster provisions nodes that become Ready and run pods, including GKE system DaemonSets.
- [ ] `VIA_KUBELET` behavior and timings are unchanged and covered by the existing suites.
- [ ] `nodeCreation: kubelet` restores today's behavior exactly.
- [ ] Events, metric and logs identify a stuck registration and its likely cause.
- [ ] Docs list supported modes/GKE versions, the setting, troubleshooting, the `nodes: create` privilege, and state that the mechanism is not a documented GKE contract; chart README updated.
- [ ] e2e runs the suites on a 1.37 cluster in both modes and on every supported minor; `MaxK8sVersion` revisited.
- [ ] A support request to Google about non-MIG VMs under control-plane node creation is filed and linked.

---

## Implementation Phases

Phases 1 to 3 are separate PRs, in this order.

### Phase 0 - Validation prerequisites (with this proposal)

`min_master_version` in Terraform; the channel image-selection spec skips when the channel lacks the cluster minor.

### Phase 1 - Detection and visibility

`NodeCreationMode`, startup and mode-change logging, a clear error and event for launched, unregistered claims past the registration grace in `VIA_CONTROL_PLANE`, docs stating the limitation. Shippable alone: it turns silent 15-minute churn into an actionable message.

### Phase 2 - Enrollment record

The CSR controller records `karpenter.k8s.gcp/kubelet-enrolled-at` on the NodeClaim. Coordinate with #795.

### Phase 3 - Node creation

Typed `kube-labels` reader, the `nodecreation` controller, `nodeCreation` setting, RBAC `nodes: create`, metrics, events, security docs.

### Phase 4 - Validation and rollout

Control-plane e2e profile (section 9: tier 1 first, then both profiles in CI), the `registration` spec and diagnostics, `MaxK8sVersion` update, migration note, release notes. Decide, from data, whether always creating the Node in every mode should replace mode detection (see Alternatives).

---

## Migration

No action on `VIA_KUBELET` clusters. Operators on `VIA_CONTROL_PLANE` clusters upgrade the chart; the RBAC rule `nodes: create` is part of the chart. Operators who manage the ClusterRole themselves must add `create` for `nodes`. `nodeCreation: kubelet` restores the previous behavior.

---

## Alternatives Considered

### Always create the Node, in every mode

Removes mode detection and flip races, and exercises one path. Rejected for the first release: it changes registration for every existing cluster, and `--node-labels` are not applied to a pre-existing Node, so any label the provider fails to reproduce would regress healthy clusters. Phase 4 revisits this once data exists.

### Create the Node inside `CloudProvider.Create`

Simple, but couples VM launch to Kubernetes writes before the kubelet has authenticated and breaks the invariant that a Node follows an authenticated kubelet. A separate controller is idempotent, retryable and testable.

### Use the core `NodeLifecycleHook`

The hook only gates when registration completes; it cannot create the Node.

### Carry the Node template in a NodeClaim annotation, recompute everything, or parse `kube-env`

An annotation is a mutable, user-visible copy of bootstrap data. Recomputing labels can diverge from the launched VM (inherited readiness labels vary by source pool). Parsing `KUBELET_ARGS` from `kube-env` is secret-bearing (kubelet credentials live in the same item) and omits `kube-labels`-only labels. Reading the non-secret `kube-labels` item and recomputing the few taints from shared launch code avoids all three.

### Wait for Google to support non-MIG VMs

Unknown timeline and mechanism. Pursued in parallel (open question 1), but users on default-CP clusters need a working path now.

### Block provisioning on `VIA_CONTROL_PLANE`

Cheap, and shipped as Phase 1 for visibility, but it leaves the default mode of new clusters unsupported.

---

## Future Direction

- Drop the `auto` fallback and the setting if Google supports third-party VMs natively.
- Reuse the controller to tolerate other GKE registration changes.

---

## Open Questions

1. **Supported path from Google**: does `gcp-controller-manager` support standalone VMs in control-plane mode, or will admission eventually restrict other Node creators or reject a kubelet `Create` before the `AlreadyExists` check? Open; to be raised with GKE support.
2. **Label reconciliation**: GKE documents that labels and annotations can lag Node creation. Does GKE reconcile `kube-labels` (including `*.node-restriction.kubernetes.io` labels) onto an adopted Node, and when? In the manual proof of concept some labels (disk-type, cluster location) appeared on an adopted Node that the author had not set, while kubelet `--node-labels` were not applied. Open; e2e must assert ordering and a diff against a MIG node.
3. **Certificate rotation in control-plane mode**: `system:node:<name>` renewal CSRs were not exercised. Open.
4. **Grace period**: 45 s from enrollment is based on observed timing; tune after e2e data.
5. **Reported default**: new 1.37.2 clusters were `VIA_KUBELET` despite the release note. Re-verify after the release note's version threshold is broadly rolled out.
