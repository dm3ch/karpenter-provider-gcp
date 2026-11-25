# Proposal: GKE 1.37.2+ default CPU and memory reservation formula

- **Status**: Implemented
- **Authors**: @dm3ch
- **Created**: 2026-11-25
- **Related Issues**: [#800](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/800)

---

## Summary

Use the CPU and memory reservation formula GKE applies to node pools running 1.37.2-gke.1325000 or later for nodes whose selected bootstrap release is at or after that release, and keep the earlier formula for older releases and for images whose operating system is unknown. The formula is chosen per instance type from the same image selection that later launches the node, so the scheduler's view of allocatable always equals the reservation written into the node's kubelet configuration.

No API change is required.

---

## Motivation

### Problem Statement

The provider reserves CPU and memory with the tiered percentages GKE used before 1.37.2-gke.1325000. GKE's [node sizing page](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/plan-node-sizes#memory_and_cpu_reservations) describes a newer default for node pools on that release or later, introduced in the [1.37 release notes](https://docs.cloud.google.com/kubernetes-engine/docs/release-notes#other_changes_in_137):

- Memory: `Min(EarlierMemoryReservation, 15 MiB × MaxPodsPerNode + BaseOverhead)`, where `BaseOverhead` is 500 MiB on Container-Optimized OS and 700 MiB on Ubuntu. The 100 MiB eviction threshold stays separate, as before.
- CPU: the total reservation is limited to 1000 mCPU for non-fractional machines. Shared-core E2 machines keep their fixed 1060 mCPU.

On a 16 GiB node with the default 110 pods the memory reservation falls from 2662 MiB to 2150 MiB (COS). Karpenter-provisioned nodes on 1.37.2+ reserve more than an equivalent GKE node pool, which lowers allocatable and can raise node counts for memory-heavy workloads. The behavior is conservative, so this is an efficiency and parity request rather than a correctness defect.

### Goals

- Match GKE's default reservation on nodes running the new formula's releases.
- Keep the scheduler's allocatable estimate equal to the node's kubelet reservation.
- Leave nodes on earlier releases, and every explicit `kubeReserved`/`systemReserved` override, unchanged.

### Non-Goals

- GKE's `reservedResourcesConfig` (1.37.2+). `kubeletConfiguration.kubeReserved` and `systemReserved` already override the defaults.
- The image-streaming memory buffer, which the provider does not model today.
- Changing the ephemeral-storage reservation, eviction thresholds, or the 100m kube-proxy scheduling overhead.
- Re-provisioning running nodes.

---

## Proposal

### Design Details

#### Formula selection

The bootstrap release of a launch is `status.images[].kubernetesVersion` of the image `imagefamily.FindImage` selects for the instance type's requirements ([proposal 0013](0013-release-aligned-bootstrap.md)). A release is at or after the threshold when its `<major>.<minor>.<patch>` and GKE build compare greater than or equal to `1.37.2-gke.1325000` as a four-number tuple, so `1.37.3-gke.100` qualifies and `1.37.2-gke.1324999` does not. A release that is not in `-gke.<build>` form cannot be placed against a GKE build and keeps the earlier formula.

`BaseOverhead` follows `GCENodeClass.ImageFamily()`: `ContainerOptimizedOS` 500 MiB, `Ubuntu` 700 MiB. A `Custom` family (raw image IDs without an explicit `spec.imageFamily`) keeps the earlier formula, because the provider cannot tell which operating system the image runs and a wrong base overhead would under-reserve. Operators who set `spec.imageFamily` opt into the matching base overhead.

`MaxPodsPerNode` is `GCENodeClass.GetMaxPods()`, the value written to the kubelet's `--max-pods`. `podsPerCore` can lower the effective pod count below it; the larger configured value is used so the reservation is never lower than GKE's for the same maximum.

#### Scheduler and kubelet consistency

Instance-type overhead is computed per instance type, while the release is resolved per launched node. Both use one function, `FindImage`, over the same `status.images`:

1. `List` builds the static instance types for a NodeClass. When the family has a base overhead and at least one image row qualifies, it builds a second, density-aware variant of each type next to the earlier one. The image family's base overhead and the existence of a qualifying row are part of the static cache key, so a channel promotion, an image rollback or a family change cannot be served from shapes cached for the previous state.
2. After requirements are computed for each type, `FindImage` selects the image as launch will. Types whose image qualifies use the density-aware variant; the others, such as an arm64 image still on an older release, use the earlier one.
3. At launch the kubelet `kubeReserved` CPU and memory are written from the selected instance type's `Overhead`, not recomputed from the release. The node therefore reserves exactly what the scheduler assumed for that type, even if `status.images` refreshes between scheduling and launch. Karpenter learns the real allocatable once the node registers.

The release remains a drift non-trigger, as in proposal 0013: a release-only status change does not replace running nodes. A channel promotion that changes the image does, as today, and the replacement nodes use the new formula.

#### Code layout

`pkg/utils/reservation.go` holds the pure formula and release gate next to the earlier formula. `pkg/providers/instancetype` owns variant selection because it already owns NodeClass-to-instance-type conversion and the cache. `pkg/metadata` is unchanged.

---

## Migration

Nodes created from this release on report more allocatable memory on qualifying releases, which may lower node counts. Existing nodes are unchanged until replaced. Operators who want the earlier values keep them with explicit `kubeletConfiguration.kubeReserved` entries. See [MIGRATION.md](../MIGRATION.md).

## Test Plan

Hermetic unit tests cover the release boundary at 1.37.2-gke.1325000 including later patches and non-GKE strings, the memory formula for COS and Ubuntu and for small, high-density and low-density machines, the CPU cap and shared-core exception, per-architecture image selection, custom images, cache isolation across release and family changes, and `maxPods` and `kubeReserved` overrides. Real node registration with a 1.37.2+ image is useful integration coverage for the observed allocatable but requires separate approval for any live environment; this proposal does not claim it.

## Alternatives Considered

- **Include the release in the instance-type cache key:** the release depends on the image row selected by requirements, which are computed after the cached static types, so one key per release combination would be needed. Caching both variants and selecting after requirements avoids the combinatorics.
- **Switch only when every selectable image qualifies:** delays parity for mixed architectures indefinitely and is unnecessary because selection is per instance type.
- **Recompute the reservation from the release at launch:** would let the node's kubelet configuration disagree with the scheduler's allocatable.
- **Apply the new formula to custom images with a conservative 700 MiB base:** changes allocatable for images whose operating system and release are unverified.
