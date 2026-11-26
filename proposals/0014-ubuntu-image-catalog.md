# Proposal: Ubuntu image catalog for GKE 1.36+, `osVersion` and per-patch images

- **Status**: Implemented by [#802](https://github.com/karpenter-gcp/karpenter-provider-gcp/pull/802)
- **Authors**: @dm3ch
- **Created**: 2026-11-21
- **Related Issues**: [#789](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/789) (follow-up: [proposal 0013](0013-release-aligned-bootstrap.md) left the Ubuntu catalog migration out of scope)

## Summary

GKE publishes Ubuntu node images in `ubuntu-os-gke-cloud` before Kubernetes 1.36 and in `gke-ubuntu-node-images` from 1.36 ([node image source projects](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-images#node_image_source_projects); the release notes and 1.36 changelog do not mention it, so that page is the only official source). Karpenter reads only the former. This proposal:

- selects the image project from the cluster's minor and replaces the per-series regex copies with one release table;
- adds `imageSelectorTerms[].osVersion` so `family: Ubuntu` chooses the release (`"22.04"`, `"24.04"`, `"26.04"` or `default`) without a family per release;
- prefers, on 1.36+, the per-patch `-c-pre` image GKE boots its own Ubuntu pools with, which starts nodes about 2.5 times faster;
- keeps drift stable across the project move and the packaging change.

## Motivation

A catalog listing on 2026-11-21 showed:

- Plain images `ubuntu-gke-2404-1-3{6,7}-<arch>-vYYYYMMDD[a]` exist in both projects with identical names. Resolution works today on 1.36 and 1.37, but only the new project is documented for 1.36+, and the old one can stop receiving builds without notice. Staying on it would then leave `latest` silently stale. The cost is that environments with trusted-image-project policies or VPC Service Controls must allow the new project.
- Ubuntu 26.04 images exist for 1.37 and 1.38 only, and no 24.04 image exists for 1.38. 22.04 images stop at 1.31.
- New variant suffixes appear (`-hwe`, `-64k`, `-acs`, besides `-tpu`, `-cgroupsv1`, `-linux64k`).
- The new project also holds per-patch `gke-<version>-gke<build>-ubuntu-<release>-<minor>-<arch>-vDATE[-variant]-c-pre` images. Their embedded release does not always agree with the `1-NN` token, so the name is not a reliable source for the release.
- Drift compares the node's boot image to `projects/<project>/global/images/<name>` by suffix, so moving the resolved project would replace every Ubuntu node.

### Goals

- Resolve Ubuntu images from the project GKE documents for the cluster's minor.
- Select the release with a field and support 26.04 without another copy of the 22.04/24.04 branching.
- Start Ubuntu nodes from the image GKE itself boots, without extra catalog quota.
- Replace no existing node because of the project move or the packaging change.
- Fail clearly, naming the release and project, when GKE has no image of the selected release for the minor.

### Non-Goals

- Dropping Ubuntu 22.04 or removing `Ubuntu2204`/`Ubuntu2404`, which stay working and deprecated; removal follows the API compatibility policy.
- Release-channel selection for Ubuntu, a per-patch image of any GKE version other than the master's, or deriving the Ubuntu release from `-c-pre` names. Ubuntu keeps the API-server snapshot behavior of proposal 0013.
- Substituting another release or image project when the selected one has no image.
- Changing the `Ubuntu@...` aliases (they stay Ubuntu 24.04) or the default boot disk type.

## Proposal

### Project selection and release table

For cluster Kubernetes `1.N`, `N >= 36` selects `gke-ubuntu-node-images`, otherwise `ubuntu-os-gke-cloud`. Only that project is listed, with no cross-project fallback, so a blocked project fails visibly (`ImageResolutionFailed`).

A table keyed by release token (`2204`, `2404`, `2604`) holds the clean-name allowlist per architecture (22.04 amd64 has no arch token) and an optional hint appended to failures (22.04: images only through 1.31). Allowlists anchor on the version end, so variants and `-c-pre` names are never candidates of the plain lookup. A new release is one row.

### `osVersion`

`imageSelectorTerms[].osVersion` (pattern `^(default|[0-9]{2}\.[0-9]{2})$`):

- `family: Ubuntu` takes it; every other family rejects it. Omitted means `default`, as for a GKE node pool that names only the image type. Proposal 0002 rejected a bare `Ubuntu` family so Karpenter would not pick an OS release silently. `default` meets that concern because it follows only GKE's documented rule, never "newest with images", and a fixed release never changes on its own.
- `version` keeps meaning the build pin (`latest` or `vYYYYMMDD[a-z]`); `channel` stays COS-only.
- The format is a pattern, not an enum, so a new release needs no CRD change (CRDs upgrade separately from the controller). A release Karpenter does not know fails at resolution listing the supported ones.
- `Ubuntu2404` and `Ubuntu2204` are deprecated and resolve exactly like `Ubuntu` with `osVersion: "24.04"`/`"22.04"`; no `Ubuntu2604` family is added. Switching spellings drifts nothing (`imageSelectorTerms` is outside the NodeClass hash and drift compares image builds).

### `osVersion: default`

GKE's default release logic is internal. The only documented rule is "from 1.32.1-gke.1002000 the default Ubuntu image moves from 22.04 to 24.04", and the API exposes only the image type. `default` replicates that rule from a small sourced table (22.04 up to 1.32.0, 24.04 from 1.32.1):

- Above `ubuntuDefaultVerifiedMinor` (1.37, the range in which GKE documents no further change) it uses the one row marked as the fallback, GKE's current documented default (24.04), and logs a warning. Below the supported minimum it uses the oldest row. 26.04 is never inferred from image availability.
- If the chosen release has no image for the cluster (24.04 on 1.38), resolution fails naming the release and project.
- It follows GKE across cluster upgrades: crossing 1.32.1 changes the release, as for a GKE-managed pool.
- Release gate: a unit test requires `ubuntuDefaultVerifiedMinor` to be at least the minor of `MaxK8sVersion`, exactly one fallback row, and that row to be the newest. Raising the maximum fails it until the GKE release notes are checked for a default change, a row added if needed, and the constant raised (`RELEASE.md` lists the step). Per-patch images are a cross-check: `gcloud compute images list --project=gke-ubuntu-node-images --filter="name~'^gke-1370-.*-ubuntu-'"` shows which release GKE assigns, and shows 24.04 through 1.37 with 26.04 only beside it.
- The release comes from the same API-server snapshot as image discovery and bootstrap, so release, project and bootstrap version cannot disagree.

A runtime source for the default does not exist: `getServerConfig` returns only `defaultImageType`, the GCS image maps cover COS only, and the per-patch names are ambiguous on 1.37 and 1.38, where versions carry both 24.04 and 26.04, and absent before 1.36. The table stays the authority.

### Per-patch images

On 1.36+, GKE gives each Ubuntu node pool a per-patch image. A throwaway pool on a 1.36.4-gke.2046003 cluster (2026-11-22) had `config.nodeImageConfig` and a VM boot disk both naming `gke-1364-gke2046003-ubuntu-2404-1-36-amd64-v20260914-c-pre`. It is the plain base image of that build with the release's node components preloaded (12 GB disk, 10 GB archive, against 10 GB and 2.5 GB). The legacy project has none.

Resolution prefers it, per architecture:

- **Scope:** only `gke-ubuntu-node-images` and only the master's exact `<version>-gke.<build>`, with no fuzzy matching to another patch. Otherwise, including older minors or a master patch released moments ago, the plain image is used and nothing else changes. A lookup that fails with an API error fails resolution instead: falling back would pick the plain image, whose base build can be newer, and drift running nodes. The NodeClass then keeps its previous images, reports `ImagesReady=False` and retries.
- **Boot disk size:** the per-patch image needs a 12 GiB disk (the plain image 10 GiB) and GCE rejects a smaller one, while the API accepts `sizeGiB >= 10`. A NodeClass whose explicit boot disk is smaller than the image keeps the plain image; without an explicit boot disk the provider default (larger) is used. The size is part of the resolution cache key.
- **Selection:** clean names of the right release and architecture, READY and not deprecated; variants are excluded. GKE often publishes two base builds for one version, together (about a third of 1.36 versions, an older and a newer base). GKE does not document which one a pool gets and it cannot be observed without a pool of that version, so Karpenter chooses the newest base, which carries the latest OS patches, and `version` pins a specific one. A pin selects exactly its date, with the plain image of that date if there is no per-patch image.
- **Freshness:** the base is the one GKE published for the master version, so it follows GKE's patch cadence and can be older than the newest plain build. Pin `version` to choose a build.
- **Bootstrap:** unchanged. The bootstrap release is the API-server snapshot, which equals the image's GKE version by construction.
- **Catalog quota (follows #644):** no server-side filter, newest first, partial responses. The plain lookup stops on its first page. The per-patch lookup is a separate scan bounded to 8 pages (4,000 images) that keeps reading only 24 hours past the newest match to see sibling bases. Results are cached for an hour (misses for 10 minutes) by project, release, master version and pin. Each cached image is rechecked with a single get on every resolution, and the scan is repeated if it was deleted or deprecated.

**Measured startup** (shared 1.36.4 E2E cluster, `n2-standard-2`, on-demand, identical base build `v20260914`, images selected by ID; VM creation to node Ready):

| Setup                                       | Median | Range      | Runs    |
|---------------------------------------------|--------|------------|---------|
| Plain image, provider-default boot disk     | 204 s  | 200 to 207 | 5 warm  |
| Per-patch image, provider-default boot disk | 80 s   | 77 to 80   | 4 warm  |
| Plain image, `pd-balanced` boot disk        | 189 s  | 177 to 196 | 3       |
| Per-patch image, `pd-balanced` boot disk    | 57 s   | 50 to 58   | 3       |
| GKE-native Ubuntu pool, `pd-balanced`       | 52 s   | 51 to 61   | 3 nodes |

The per-patch image is about 2.5 times faster (NodeClaim creation to pod Ready: about 250 s to 136 s) because `configure.sh` finds the components preloaded (13 "preloaded" lines, as on the GKE node) instead of downloading 47 items. The remaining gap to GKE is the provider's default `pd-standard` boot disk, which starts `configure.sh` about 20 s later; with `disks[].category: pd-balanced` Karpenter is within about 6 s of GKE. Changing that default is out of scope, and the docs recommend `pd-balanced` for startup. Samples are small and from one environment.

### Drift

A node's boot image counts as the published image when the paths match, or when both are Ubuntu GKE images of the same OS build: release, image minor, architecture and base date. The plain image, the per-patch image of that build and either Ubuntu project are therefore one image, and a project move or a switch to the per-patch image of the same build replaces no node. A different base build, architecture or release still drifts. Because the master's per-patch base can be older than the newest plain image, a `latest` NodeClass can see one replacement when the controller is first upgraded on 1.36+ (see Migration). A pin is unaffected.

## Risks and Mitigations

| Risk                                                             | Likelihood             | Impact                                        | Mitigation                                                                                    |
|------------------------------------------------------------------|------------------------|-----------------------------------------------|-----------------------------------------------------------------------------------------------|
| Controller cannot read `gke-ubuntu-node-images` (policy, VPC-SC) | Medium                 | Ubuntu `ImagesReady=False` on 1.36+           | `MIGRATION.md` and docs name the project; the failure names it.                               |
| Google stops publishing a release for a minor (24.04 on 1.38)    | High (certain on 1.38) | Existing NodeClass cannot resolve             | Clear error; docs list availability; no silent release switch.                                |
| A third spelling of one selection adds API surface               | Medium                 | Confusing docs, more validation               | Old families frozen and deprecated; docs show only the new shape.                             |
| Names diverge between the two projects                           | Low                    | Spurious or missed drift                      | Name equality is unit-tested; revisit if Google changes naming.                               |
| Per-patch base older than the newest plain image                 | Medium                 | One replacement of `latest` nodes on upgrade  | Documented; disruption budgets; pin `version`.                                                |
| Several bases for one version; GKE's assignment is unobservable  | Medium                 | "Newest" may differ from the base GKE assigns | Deliberate choice, documented; a pin selects exactly.                                         |
| Per-patch lookup costs catalog quota                             | Low                    | Extra list calls                              | No filter, 8-page bound, hour-long cache, cached images rechecked.                            |
| Ubuntu 26.04 bootstrap differs from 24.04                        | Medium                 | 26.04 nodes fail to register                  | Documented as not validated live (alpha minors only); the E2E spec self-skips without images. |

## Test Plan

### Unit Tests

- Project and prefix per minor (1.31, 1.35, 1.36, 1.37, 1.38) and an unparseable version; only the selected project is listed.
- Per release and architecture: clean names (with letter suffix) accepted; variants, other releases, `-c-pre` and non-READY images rejected; 26.04 beside `-hwe`/`-tpu`/`-64k`/`-acs`. A missing release fails with project and release (and the 22.04 hint); an unknown `osVersion` lists the supported ones.
- Routing: `Ubuntu`+`osVersion`, `Ubuntu2404`, `Ubuntu@latest` and `Ubuntu2404@latest` resolve identically for 24.04.
- CRD admission (generated CEL): `Ubuntu` admits an omitted `osVersion` and `default`; other families reject it; malformed values are rejected; deprecated families still admit.
- `default`: boundaries 1.31, 1.32.0 and 1.32.1; out-of-range versions use the fallback (above) or oldest row (below); 1.31 gives 22.04 although 24.04 images exist; 1.37 gives 24.04 although 26.04 images exist; 1.38 fails clearly; one fallback row; `ubuntuDefaultVerifiedMinor` at least the `MaxK8sVersion` minor.
- Per-patch: preferred for the exact master version per architecture; variants, other releases and other versions excluded; newest base for `latest`; exact date for pins with plain fallback; plain fallback when absent, for non-GKE versions and for the legacy project; plain image when the boot disk is smaller than the per-patch image, without leaking through the cache; no server-side filters; page bound; a cache hit avoids rescanning but rechecks the images, and a deleted or deprecated one is replaced; a failed scan fails resolution.
- Drift: plain and per-patch images of one build, from either project, are the same; another base date, architecture, release or minor is not.

### E2E / Integration Tests

- The Ubuntu provisioning and pinning suites resolve from the new project on a 1.36+ cluster and accept the per-patch image for `latest`.
- An `osVersion: "26.04"` spec runs where the cluster's minor has 26.04 images and self-skips otherwise. The persistent E2E cluster runs 1.36, so 26.04 bootstrap is not validated by this change.

## Migration

Allow the controller's project and network policy to use `gke-ubuntu-node-images` on GKE 1.36+. Upgrade CRDs before the controller to get `osVersion`. Existing `Ubuntu2404`/`Ubuntu2204` NodeClasses keep working and can move to `family: Ubuntu` with `osVersion` at any time without drift. Before upgrading a cluster to a minor with no image of the NodeClass's release, change `osVersion` to one that has images. Existing nodes are not replaced by the project change. On 1.36+, `latest` NodeClasses may replace Ubuntu nodes once when the per-patch base is older than the newest plain build; use disruption budgets or pin `version`.

## Alternatives Considered

- **Probe both projects and prefer the first hit:** rejected; it hides blocked projects and makes the catalog depend on transient publication state.
- **Any GKE patch, or the release decoded from `-c-pre` names:** rejected; the embedded release is unreliable and a patch other than the master's would change pin and channel semantics.
- **Derive the default release from GKE data at runtime:** rejected for the reasons under `osVersion: default`; the sourced table stays the authority.
- **Default the boot disk to `pd-balanced`:** rejected here; it would remove about 20 s of startup but changes cost and is a separate decision.
