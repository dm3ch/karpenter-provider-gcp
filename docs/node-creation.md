# Node creation

GKE can create the Kubernetes `Node` object from its control plane instead of letting the kubelet register itself. This page explains how Karpenter provisions nodes in both modes, what it creates itself, and who removes the `Node` afterwards. Design background and open questions: [proposal 0016](../proposals/0016-control-plane-node-creation.md).

## Background

`nodeCreationConfig.nodeCreationMode` on a GKE cluster is either:

- `VIA_KUBELET`: the kubelet creates its own `Node` after it obtains a client certificate.
- `VIA_CONTROL_PLANE`: GKE's `gcp-controller-manager` creates the `Node`. The GKE release notes make this the default for new 1.37+ clusters; the GKE documentation still labels it Preview.

In `VIA_CONTROL_PLANE` mode GKE does not create a `Node` for a VM outside its own node pools. A Karpenter VM boots and its kubelet gets a certificate, but no `Node` appears, so the NodeClaim times out after 15 minutes and is replaced. Karpenter therefore creates the `Node` itself.

## How a node comes up

1. **Launch.** `CloudProvider.Create` starts the VM and records the label set the VM was launched with in the NodeClaim annotation `karpenter.k8s.gcp/node-labels`, without `karpenter.sh/registered` and `karpenter.sh/initialized`.
2. **Enrollment.** The [CSR controller](security/csr-approval.md) approves the kubelet's bootstrap certificate request and records the time in the annotation `karpenter.k8s.gcp/kubelet-enrolled-at`.
3. **Node creation.** The node creation controller creates the `Node` for a NodeClaim that has launched, is enrolled and has no `Node` yet. It never updates or patches a `Node` that already exists.
4. **Adoption and registration.** The kubelet adopts the existing `Node`, the cloud controller manager initializes it, and core Karpenter registers the NodeClaim as usual.

Karpenter waits for enrollment instead of creating the `Node` at launch, so that a `Node` follows an authenticated kubelet. A VM that never boots, never enrolls or is refused never gets a `Node`, and a NodeClaim deleted during boot leaves none behind. The wait adds about a second.

## What the Node contains

- the labels from `karpenter.k8s.gcp/node-labels`, filtered the way a kubelet would be: reserved `kubernetes.io` and `k8s.io` keys that a kubelet may not set (including `node-role.kubernetes.io/*`) are dropped, node-restriction labels are kept
- the registration taints (`karpenter.sh/unregistered`, GPU taints where configured) and the cloud-initialization taint
- `spec.providerID` and the annotation `karpenter.k8s.gcp/created-by: nodecreation-controller`
- no owner reference and no finalizer; core adds both at registration

## When Karpenter creates the Node

There is no setting. Karpenter creates the `Node` only when the GKE cluster reports `nodeCreationConfig.nodeCreationMode = VIA_CONTROL_PLANE`. On a `VIA_KUBELET` cluster it never creates a `Node`, and nothing changes from earlier versions.

The cluster's mode is cached for up to 30 minutes, so a switch to `VIA_CONTROL_PLANE` is not seen immediately. When an enrolled NodeClaim still has no `Node` 5 minutes after enrollment, Karpenter re-reads the mode once for that claim and creates the `Node` if the cluster now reports `VIA_CONTROL_PLANE`. The wait is a refresh delay, not a fallback: on a `VIA_KUBELET` cluster the claim keeps waiting for its kubelet.

## Lifecycle and deletion

Karpenter never deletes a `Node` it created.

| Situation                                                                        | What removes the `Node`                                                                                                                                              |
|----------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Registered node, NodeClaim deleted (consolidation, expiry, drift, manual delete) | Core Karpenter: its termination finalizer and the owner reference drain the node, delete the VM and remove the `Node`, as on any other cluster                       |
| NodeClaim deleted before registration                                            | Core's NodeClaim finalizer deletes Nodes by `providerID`; the cloud controller manager removes a `Node` whose VM is gone if creation raced with deletion             |
| Registered node whose VM is deleted outside Karpenter                            | The cloud controller manager's node lifecycle controller deletes the `Node` because its VM no longer exists (about 80 s in testing); core then removes the NodeClaim |

## Permissions and limits

- Creating Nodes needs the `create` verb on `nodes`. The chart grants it; operators who manage the ClusterRole themselves must add it.
- The mechanism relies on the kubelet adopting a pre-existing `Node`. That is observed behavior on GKE 1.37, not a documented GKE contract.
- A NodeClaim without a usable `node-labels` annotation is skipped with a `NodeCreationSkipped` event and replaced at the registration timeout. That covers claims launched by an older Karpenter version and VMs adopted after an interrupted launch.

## Observability

Metric: `karpenter_gcp_nodecreation_total{result="created|exists|collision|error"}`.

Events on the NodeClaim: `NodeCreatedByController`, `NodeCreationSkipped`, `NodeNameCollision`.

If a NodeClaim stays unregistered although its VM runs, see [Troubleshooting](troubleshooting.md#nodeclaim-stays-unregistered-although-the-vm-runs).
