# CSR approval security contract

Karpenter approves kubelet-client certificate signing requests (CSRs) for its GCE nodes. Approval is an authorization decision: matching a signer or certificate subject does not establish that the requester may obtain that identity.

## Approval policy

The controller validates the CSR signature, kubelet-client signer, allowed usages and subject. It rejects subject alternative names and organizations outside `system:nodes` and the supported GKE node-pool/instance organizations. Requested GKE node-pool groups must already belong to the API-authenticated requester; renewal instance groups must also already belong to that requester.

Eligible NodeClaims must reference `karpenter.k8s.gcp/GCENodeClass` and have a complete `gce://<project>/<zone>/<instance>` ProviderID matching the requested node name.

- **Bootstrap:** the requester is `kubelet-nodepool-bootstrap` or `kubelet-bootstrap`; the NodeClaim has `Launched=True`, has not registered and is not being deleted. A supplied GKE instance organization's zone must match the claim.
- **Legacy bootstrap:** the requester is exactly `kubelet`, used when Shielded GKE Nodes is disabled. It is a separate, weaker path with its own rules, described [below](#legacy-bootstrap-with-shielded-gke-nodes-disabled).
- **Renewal:** the requester is `system:node:<requested-name>` and belongs to the authenticated `system:nodes` group. The NodeClaim has `Registered=True`, and its node name and GCE instance name match the request. Self-renewal remains allowed while that registered claim is draining/deleting, so a long drain does not lose its kubelet identity.

The requester username/groups are those recorded by the Kubernetes API server, not identity assertions in the requested certificate. A failed NodeClaim read prevents approval. Already approved or denied CSRs are not changed. Unrelated requests remain pending for their proper approver or an administrator.

The Google-maintained [cloud-provider-gcp implementation](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gcp-controller-manager/node_csr_approver.go#L62-L64) identifies `kubelet-bootstrap` as a TPM bootstrap requester. This historical reference establishes the identity's provenance, not an exhaustive contract for current GKE versions. Karpenter accepts it under the same NodeClaim-based policy as `kubelet-nodepool-bootstrap`; it does not implement Google's TPM attestation and SubjectAccessReview flow. Supporting this additional identity expands the bootstrap principals eligible for approval, not the requested certificate subjects or groups.

Neither bootstrap identity may re-enroll an already registered NodeClaim. This compatibility fix does not add support for plugin reissuance authenticated with bootstrap credentials after registration; authenticated node self-renewal remains a separate path.

## Legacy bootstrap with Shielded GKE Nodes disabled

The exact requester `kubelet` is the identity GKE uses to enroll nodes in a cluster with Shielded GKE Nodes **disabled**. It carries no TPM proof. It is approved only while the cluster's Shielded GKE Nodes setting is read as disabled, and only for initial enrollment of one pending NodeClaim. It is never a fallback for the other bootstrap identities, and neither the setting nor this path is consulted for them or for self-renewal. The original requester must also be allowed to `create certificatesigningrequests/nodeclient`, checked with a `SubjectAccessReview` (SAR) for the CSR's username, UID, groups and extras.

The request must satisfy a stricter shape than the other paths:

- `request` is parsed as the signer parses it: the first PEM block, type `CERTIFICATE REQUEST`, with no whitespace trimming. Nothing but whitespace may follow it; a duplicate, any extra block (including a TPM envelope) or trailing text is rejected. Text that `pem.Decode` skips before the block is skipped by the signer too, so the approved request is the one that is signed.
- The subject is `CN=system:node:<name>` with exactly one organization, `system:nodes`: no node-pool, instance or other organization and no other subject attribute.
- The usages are exactly `digital signature` and `client auth`, optionally with `key encipherment` (the pair Google's legacy validator accepts); no other subset and no duplicates.
- The signer, signature and no-SAN checks are shared with the other paths.

The NodeClaim must be launched (`Launched=True`), not registered (`Registered` not true, no node name), not deleting, and record a valid non-zero `karpenter.k8s.gcp/instance-numeric-id`. A registered or draining claim never uses this path. The controller reads the live VM fresh from Compute (no instance cache, no attestation key) and requires its project, zone, name and numeric instance ID to match the claim's ProviderID and recorded incarnation, and its NodeClaim-UID ownership label to name this NodeClaim. An unlabelled VM is not adopted or backfilled.

The setting is read from the GKE API with an uncached request after the checks above and the SAR, immediately before approval, and again on every attempt. An enabled setting means no approval; an absent, unreadable or failed answer is unknown and leaves the request pending for retry. A response with `shieldedNodes: {}` is a present, disabled setting (the API omits the proto3 default `false`); a missing `shieldedNodes` message is unknown, not disabled. There is no transaction across GKE, Compute and Kubernetes: this removes the application-cache window but not every race, and NodeClaim lifecycle comes from the reconcile's snapshot.

**This path gives weak requester-origin assurance.** The VM association is not a cryptographic proof that the requester is that VM. The legacy credential is shared by the cluster's bootstrapping nodes, so a holder can request a certificate for another eligible pending NodeClaim. It is the consequence of an operator disabling Shielded GKE Nodes. With Shielded GKE Nodes enabled the kubelet uses a TPM bootstrap identity, whose approval is unchanged by this path.

The [native evidence in Proposal 0015](../../proposals/0015-gce-instance-bound-csr-approval.md#legacy-un-attested-bootstrap) covers one COS/GKE enrollment profile, not every version, image or architecture. The SAR for this requester and the candidate controller's own legacy approval have not been validated against real GKE.

## Enrollment record

After a successful bootstrap approval the controller annotates the approved NodeClaim with `karpenter.k8s.gcp/kubelet-enrolled-at` (RFC 3339 time), once. The [node creation controller](../node-creation.md) creates a `Node` only for claims carrying it, so a `Node` created by the provider still follows an approved kubelet certificate request, and a VM that never boots, never enrolls or is refused never gets one; whatever verification this controller applies to bootstrap requests gates the annotation too. It is written only after the approval write succeeds. If the annotation write fails, the next reconcile retries it for CSRs this controller approved, under the same authorization. Renewals never write it. The annotation is a record, not an authenticator: anyone who can patch NodeClaims can set it, as they can create NodeClaims.

Creating `Node` objects needs `create` on `nodes`, which the chart grants to the controller; the node identity gains no permission.

## Remaining trust boundary

**This policy does not authenticate the requester as the actual GCE VM.** GKE source pools share a bootstrap username. A holder of one of those bootstrap credentials can still request a pending Karpenter node's identity before registration. Lifecycle, name and zone checks narrow that exposure but do not close it. A Compute lookup confirming an attacker-supplied instance ID would not, by itself, prove requester identity either.

Closing that boundary requires separately designed and validated instance-bound enrollment, including real bootstrap and certificate rotation. This change does not implement such a protocol or revoke certificates already issued. Deleting a CSR does not revoke its certificate. Another approver may also approve requests independently; these controller checks are not a claim about every cluster approval path.

## Regression tests

Run the focused offline authorization tests:

```sh
go test -race ./pkg/controllers/csr
```

They cover approval decisions, the legacy request shape and lifecycle, the live-incarnation and per-attempt policy reads, the SAR query and the approval API write using a local HTTP test endpoint and a small read-client double. Fuzz targets in `pkg/controllers/csr/fuzz_test.go` cover the code that reads the request field; `go test` runs their seeds and `make fuzz` explores them. They do not exercise real API admission, CA signing or GKE joining/rotation. Real GKE bootstrap and rotation must be validated separately before release, in an explicitly approved environment.

## Chart certificate permissions

The controller's chart grants:

- `get`, `list`, `watch` on CSRs;
- `update` on the approval subresource;
- `approve` only for `kubernetes.io/kube-apiserver-client-kubelet`;
- `create` on `subjectaccessreviews`, used by the legacy path to query the API server with the original requester identity.

The legacy path reads the cluster's Shielded GKE Nodes setting with `container.clusters.get`, which the controller's role already grants.

These permissions are bound to the controller ServiceAccount. External cluster roles, group memberships, bindings and aggregation inputs require a separate authorized exposure audit. Existing gosec/CodeQL checks and Greptile review guidance are complementary, not proofs of correct authorization.
