# Proposal: GCE instance-bound kubelet CSR approval

- **Status**: Implemented
- **Authors**: @dm3ch
- **Created**: 2026-11-18
- **Related Issues**: [#778](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778)

## Summary

Karpenter approves kubelet client CSRs from the TPM bootstrap identities only when the requester proves possession of the GCE VM it claims. A cluster with Shielded GKE Nodes disabled can instead use proof-less `kubelet` legacy enrollment, approved through a separate, explicitly weaker path (see [Legacy un-attested bootstrap](#legacy-un-attested-bootstrap)). The proof is the native GKE TPM attestation envelope, verified against the VM attestation key that Compute reports for the live instance, and bound to the numeric VM incarnation recorded on the NodeClaim at launch or adoption.

This follows Google's [node CSR approver](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gcp-controller-manager/node_csr_approver.go) for proof parsing, signature verification, key binding, TPM structure validation and `SubjectAccessReview`, and replaces its managed-instance-group membership check with the authoritative NodeClaim/VM/cluster association available to a standalone-VM provider.

## Motivation

### Problem statement

On Shielded GKE clusters, node pools use bootstrap credentials that authenticate as `kubelet-nodepool-bootstrap` or `kubelet-bootstrap`. Before this change the controller approved any such request for a launched, unregistered Karpenter NodeClaim, so a holder of another pool's credential could request a pending Karpenter node's identity. Name, zone and lifecycle checks narrowed that exposure but did not establish that the requester was the VM.

Issue #778 asks whether instance, cluster and CSR-key association can be established throughout enrollment, renewal and drain. A signature can authenticate a VM/key relationship, while configured-cluster membership and NodeClaim ownership remain authorization policy.

### Goals

- Reuse the existing GKE TPM proof format and upstream verification semantics.
- Bind approval to the live VM incarnation and the provider-owned NodeClaim in the configured cluster.
- Preserve certificate reissuance for a registered node, including while it drains, on both the attested bootstrap path and ordinary `system:node` self-renewal.
- Document prerequisites, replay limits and the evidence that remains outstanding.
- Let the cluster's own Shielded GKE Nodes setting, not a Karpenter flag, decide whether the weaker legacy enrollment of a disabled cluster is accepted.

### Non-goals

- A custom bootstrap agent, nonce protocol, metadata-token transport or managed-control-plane change.
- Serving-certificate approval, stale-Node/Pod deletion or migration to managed instance groups.
- A global attestation-skip option, unlabelled-VM adoption or incarnation backfill, or legacy re-enrollment of a registered node.
- Claiming non-exportable kubelet keys, revoking existing certificates or securing other approvers.

## Proposal

### Feasibility evidence

The public [Shielded GKE Nodes contract](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/shielded-gke-nodes#about_shielded_gke_nodes) requires a node to belong to a cluster-provisioned managed instance group (MIG). Karpenter provisions standalone VMs, so unchanged native managed-node approval cannot apply.

Bounded research in an existing Shielded GKE **1.35.8-gke.1796000** environment (COS/amd64, `n2-standard-2`, vTPM) observed the initial enrollment against the then-deployed **v0.8.2** controller. The corrected rotation probe ran against the existing **PR #793** deployment (source `3011887c`) and observed:

- Initial enrollment authenticated as `kubelet-nodepool-bootstrap` with an authenticated node-pool group, subject organizations `system:nodes`, a GKE node-pool organization and a GKE instance organization, and the four-block envelope (`CERTIFICATE REQUEST`, `ATTESTATION DATA`, `ATTESTATION SIGNATURE`, `VM IDENTITY`). The request was approved by `KarpenterAutoApprover` and the node became Ready.
- An age-triggered native plugin cache rotation of the same registered node requested a **new** key and submitted a second attested request with the same four blocks, the same bootstrap requester class, and the same subject identity. It remained `Pending`; no competing denial was recorded.
- Both real requests were validated in memory against the authoritative Compute attestation key: PKCS#10 signature, numeric VM identity, RSA attestation signature and TPM certified-public-key binding. Equivalent checks are reproduced by the local regression fixtures.
- A `SubjectAccessReview` for the bootstrap username and its authenticated groups for `create certificatesigningrequests/nodeclient` returned `Allowed=true` with no evaluation error, matching what the controller now performs.

Ubuntu, other GKE versions and arm64 were not evaluated, and no active kubelet certificate adoption, health or drain behavior was validated. The observed rotation branch establishes protocol feasibility only: neither the native protocol nor the controller's own enforcement was validated on a candidate deployment. Temporary research resources were removed and their absence checked; no further live authority was used.

### Implemented verification flow

The [CSR approval security contract](../docs/security/csr-approval.md#approval-policy) is the authoritative policy for attested bootstrap, cluster-controlled legacy enrollment and authenticated self-renewal. It defines requester recognition, proof/key binding, original-identity SAR checks and fail-closed approval behavior. This proposal records the evidence and design rationale rather than duplicating that policy.

### Legacy un-attested bootstrap

Native evidence: on GKE **1.36.4-gke.2046003** (COS, one architecture) with Shielded GKE Nodes disabled, the node's initial client CSR came from requester `kubelet` (group `system:authenticated`), signer `kubernetes.io/kube-apiserver-client-kubelet`, usages `digital signature` and `client auth`, `CN=system:node:<name>` with the single organization `system:nodes`, ECDSA P-256, no SANs, and one plain PEM block with no proof. GKE auto-approved it and the node became Ready. The same response showed `shieldedNodes: {}`, a present message whose `enabled` is the proto3 default `false` and which the API omits (see the [`ShieldedNodes` API reference](https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.locations.clusters#shieldednodes)); an absent message is **unknown**. The pinned upstream approver includes TPM validation unconditionally and an optional separate legacy validator for `kubelet`, whose predicate accepts the two client usages, optionally with key encipherment. How managed GKE enables it internally is not public; the observed flow matches that historical principal.

The [legacy approval contract](../docs/security/csr-approval.md#legacy-un-attested-bootstrap) defines the request shape, lifecycle/VM association, authorization ordering and fresh policy-read semantics. The evidence above motivates that separate, weaker path; it is never a TPM-failure fallback or a Karpenter-controlled skip switch.

### Durable incarnation association

`InstanceID` and the GCE ProviderID remain the VM **name**. `pkg/providers/instance` now carries `NumericID` from the completed insert operation's `TargetId` (or, when both the insert response and the completed operation omit it, from an authoritative read that must show this NodeClaim's ownership label) and from ownership-checked adoption. `Get`/`List` inventory leaves the trusted `NumericID` unset: observing an ID by name does not establish ownership. `Create` neither launches without a server-reported ID nor returns a VM whose incarnation it cannot vouch for: a legacy name-only adoption fails with an actionable `InstanceIncarnationUnknown` error instead of producing a NodeClaim that core marks launched. `Get`, `List` and adoption keep reporting those VMs so they can be cleaned up and keep renewing.

`pkg/cloudprovider` records it as the `karpenter.k8s.gcp/instance-numeric-id` annotation, which core's launch path merges into the NodeClaim, and refuses to replace an already-recorded ID with a different one. Launched VMs also carry a `karpenter-k8s-gcp-nodeclaim-uid` label; adoption rejects a same-named VM owned by another NodeClaim, and only records an incarnation when that ownership label matches. VMs created before the label existed remain adoptable by name but record no incarnation, so attested enrollment is unavailable for them until they are replaced.

Trusted controller configuration, privileged NodeClaim/annotation/status writers and VM-provisioning IAM are assumptions of this association. The label is an ownership association under that IAM, not attestation evidence.

### Lifecycle policy

See [approval policy](../docs/security/csr-approval.md#approval-policy) and [re-enrollment and rotation](../docs/security/csr-approval.md#re-enrollment-and-rotation) for the distinct initial-enrollment, drain-time re-enrollment and self-renewal requirements.

### Replay and trust limits

The proof certifies the CSR public key with the VM attestation key. It does **not** sign a controller-issued nonce, CSR UID, NodeClaim UID or cluster audience. Recorded-incarnation, lifecycle and NodeClaim checks authorize the proof in the current cluster and reject different-key, cross-instance and same-name replacement substitution; they do not turn unsigned context into a cryptographic binding. Resubmitting a proof for the same key and a still-eligible incarnation is not detected, the software-generated kubelet key is not guaranteed non-exportable, and a stolen issued certificate may still renew through the self-renewal path. Existing certificates are not revoked; deleting a CSR does not revoke one.

Other approvers operate independently, and a competing terminal denial is not overwritten.

## Prerequisites and migration

- Evidence so far: the native proof format, native cache-rotation branch and the required bootstrap `nodeclient` authorization were observed in plaintext/read-only probes on Shielded GKE **1.35.8-gke.1796000** with COS/amd64 standalone VMs and vTPM enabled. A candidate deployment has since been validated live; see [Live validation](#live-validation). Certificate rotation, drain behavior, Ubuntu, other GKE versions and arm64 remain unexercised.
- `enableVtpm: false` remains expressible but is known ineligible: without a vTPM there is no attestation key, so attested enrollment cannot succeed.
- IAM: `compute.instances.getShieldedInstanceIdentity` (new) for the controller service account, in [`deploy/iam/karpenter-controller-role.yaml`](../deploy/iam/karpenter-controller-role.yaml). The project number is read from the metadata server of the controller's own VM, which must be in the configured project; Compute's `Project.id` was measured not to equal the project number on real GKE.
- Kubernetes RBAC: the chart now grants `create` on `subjectaccessreviews`, plus the existing CSR approval permissions.
- Migration: existing NodeClaims have no recorded incarnation and cannot use attested enrollment. That includes native plugin reissuance: a registered claim launched by an earlier version cannot be re-issued through the bootstrap path, and only `system:node` continuity remains available where that path was in use. Launched, registered, non-deleting claims with a missing, empty, invalid or zero recorded ID are eligible for `InstanceIncarnationDrift` (earlier core drift reasons can take precedence) for normal dynamic/static replacement; no CSR exception or identity backfill is added. Budgets/blockers can delay replacement beyond certificate expiry, configured `terminationGracePeriod` keeps its existing pod-blocker semantics, and static pools may use terminate-first when enabled. Apply prerequisites and verify new-version enrollment before permitting fleet-wide replacement. This temporary bridge is intended for removal after v0.10 only once older installations must roll their nodes through v0.10 first; do not backfill the annotation from a name lookup. See [MIGRATION.md](../MIGRATION.md).
- Shielded GKE Nodes disabled: nodes enroll through the legacy path above, with a weaker origin guarantee. Enable it on the cluster to require proof.
- No automatic rollback: reverting to an older controller restores un-attested bootstrap approval.

## Test plan

### Implemented local regression tests

The [security contract's regression-test section](../docs/security/csr-approval.md#regression-tests) records the offline commands and covered boundaries. Tests include locally generated proofs/legacy requests, original-identity authorization, lifecycle and policy ordering, live incarnation and owned adoption, and upgrade drift. They do not replace the candidate-controller validation below.

### GKE integration coverage

`test/suites/provisioning/csr.go` provisions one COS/amd64 on-demand vTPM node through the existing helpers and asserts that the NodeClaim records the numeric instance ID of the live VM, that the VM has vTPM enabled, that the native kubelet client CSR contains the four required attestation blocks and at most one optional attestation certificate, and that it was approved by `KarpenterAutoApprover` with a certificate issued. It passed in a live `standard` preset run (20 workers) on Shielded GKE 1.36.4 at commit `3bfd55be`; in that run 67 specs passed and one unrelated arm64 launch spec failed on machine-type zone availability, so arm64 enrollment was not exercised. It does not fabricate rotation, and it assumes a Shielded GKE Nodes cluster: no spec covers native enrollment on a cluster with the setting disabled or the candidate controller's legacy approval.

### Live validation

A candidate build was exercised on a persistent Shielded GKE **1.36.4-gke.2046003** cluster with COS/amd64 on-demand nodes (vTPM enabled), upgrading from published v0.8.3:

- An old registered node had no `instance-numeric-id` annotation. After the upgrade its claim reported `InstanceIncarnationDrift`.
- A NodePool budget of `nodes: "0"` for `Drifted` held the replacement back, and the controller reported the blocking budget.
- With the budget relaxed, a replacement VM launched and recorded the live numeric ID. Its bootstrap CSR carried the four proof blocks, was approved by `KarpenterAutoApprover` with a certificate issued, and the workload moved. The old node and VM were removed.
- **The first candidate build failed this test.** It took the authoritative project number from the Compute `Project` resource, whose `id` is not the project number, so every real proof was rejected and the replacement never registered. The number now comes from the controller's metadata server. Unit fakes could not reveal this; only the live run did.

This covers one profile and one dynamic NodePool.

### Outstanding real-GKE validation

The following still requires a separately approved candidate deployment and has not been executed:

1. Exercise the installed native rotation path without exporting keys, and assert continued kubelet health.
2. Repeat under a PDB-held drain and verify the node stays healthy until release.
3. Verify tampered, different-key and cross-instance proofs cannot acquire a certificate, without claiming same-key replay rejection.
4. Evaluate Ubuntu and other supported profiles before widening the matrix.
5. Exercise static NodePools, including terminate-first where enabled, and a larger fleet rollout.

## Acceptance criteria

- [X] Pass the native initial-enrollment and rotation compatibility gate.
- [X] Review the support/migration contract and the concrete incarnation association.
- [X] Validate cryptographic interoperability and authorization locally.
- [X] Validate real GKE enrollment and replacement of an existing node on upgrade with a candidate deployment (one profile).
- [ ] Validate native certificate reissuance and drain-time re-enrollment.
- [ ] Validate the candidate controller's legacy approval (SAR for `kubelet` and enrollment on a cluster with Shielded GKE Nodes disabled) with a candidate deployment.

## Alternatives considered

- **Copy the entire Google approver:** includes MIG membership, serving certificates and stale-node cleanup with different ownership/lifecycle assumptions. Only proof verification and SAR semantics are reused.
- **Name, label or numeric-ID lookup alone:** establishes that a VM exists, not that it sent this CSR.
- **Compute instance identity JWT:** [Google-signed VM identity tokens](https://docs.cloud.google.com/compute/docs/instances/verifying-instance-identity) are useful identity evidence but are not bound to the CSR key by this bootstrap flow; transporting them would require a separately designed protocol.
- **Accept un-attested shared bootstrap requests for every requester:** preserves the original impersonation boundary and cannot be advertised as mandatory origin verification. Only the distinct `kubelet` requester is accepted, and only while the cluster's policy is fresh and disabled.
- **A Karpenter flag or Helm switch for skipping attestation:** duplicates the cluster's own control and can contradict it; the cluster setting is read instead.

## Primary references

- [Pinned upstream approver](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gcp-controller-manager/node_csr_approver.go), [TPM helper](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/pkg/tpmattest/tpmattest.go), [request producer](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gke-exec-auth-plugin/request.go), [TPM certification](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gke-exec-auth-plugin/tpm.go) and [rotation](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gke-exec-auth-plugin/cache.go).
- [Shielded GKE prerequisites and MIG requirement](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/shielded-gke-nodes).
- [Compute signing-key identity API and IAM](https://docs.cloud.google.com/compute/docs/reference/rest/v1/instances/getShieldedInstanceIdentity).
- [go-tpm](https://github.com/google/go-tpm) (Apache-2.0), used through `legacy/tpm2` for TPM structure parsing and public-object naming.
