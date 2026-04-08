# price_validate

This tool checks the quality of machineType price generation in
`karpenter-provider-gcp`. It compares the hourly prices computed by
[`instanceprice`](../../pkg/providers/pricing/instanceprice/) against two independent
reference sources simultaneously, making it easy to catch regressions in pricing
accuracy before they ship.

In case of a **Missing**, **Mismatch**, or **Extra** result, please research the
issue. Note that both reference sources can themselves contain mistakes:

- **cloud.google.com** pricing pages — scraped from embedded HTML/JSON; may lag
  behind actual billing changes or reflect a different pricing tier in some regions.
- **Cyclenerd price calculator** (`gcloud-compute.com`) — community-maintained
  reference CSV; generally accurate but not an official Google source.

In case of a mismatch between these two reference sources, it is better to check
the actual node price in the **Google Cloud Console UI** as the golden source —
it reflects actual billing and cannot be automatically scraped.

---

## How it works

Every run automatically executes three phases:

**Phase 1 — Reference prices**

Fetches both reference sources in parallel and saves them to
`data/cyclenerd.json` and `data/gcpweb.json`. Cached results are reused until
they exceed `--cache-ttl` (default 6 h).

**Phase 2 — Computed prices**

Calls `instanceprice.Client.FetchPrices()` for every region using `--workers`
parallel workers (default 16).

**Phase 3 — Compare**

Diffs computed prices against both reference sources and prints any discrepancies.

---

## Usage

```sh
go run ./hack/tools/price_validate
```

The GCP project is auto-detected via Application Default Credentials
(`gcloud auth application-default login` or `$GOOGLE_APPLICATION_CREDENTIALS`).

## Flags

| Flag          | Default  | Description                                          |
|---------------|----------|------------------------------------------------------|
| `--region`    | `all`    | Region to compare, or `all`                          |
| `--tolerance` | `0.01`   | Max fractional price diff (1%) before flagging       |
| `--no-cache`  | `false`  | Ignore all caches and fetch everything fresh         |
| `--work-dir`  | `./data` | Directory for cache and output files                 |
| `--cache-ttl` | `6h`     | Max age of reference price caches before re-fetching |
| `--workers`   | `16`     | Number of parallel workers for computing prices      |

---

## Output format

```
MISMATCH  n2-standard-8            us-central1  OnDemand computed=0.388000    cyclenerd=0.400000(+3.1%)  gcp_web=0.388000(ok)
MISMATCH  n2-standard-8            us-central1  Spot     computed=0.050000    cyclenerd=n/a              gcp_web=0.055000(-9.1%)
MISSING   c4-standard-2            europe-west1 OnDemand computed=n/a         cyclenerd=0.250000         gcp_web=0.248000
EXTRA_NEW x5-experimental-4        us-east1     OnDemand computed=0.310000    cyclenerd=n/a              gcp_web=n/a

Summary over 37 region(s): checked=1234  mismatches=1  missing=1  extra=30  extra_new=1  unavail=335  blacklisted=0  tolerance=1%
```

- `MISMATCH`   — our price deviates from a reference source by more than the tolerance. `(ok)` = agrees within tolerance; `n/a` = source has no data.
- `MISSING`    — we have no computed price for a machine that a reference source lists **and** the Compute Engine API confirms the machine type is deployed in the region. This indicates a genuine pricing gap.
- `EXTRA`      — we computed a price for a machine neither reference source lists, but the machine type is in the `knownExtras` set in `main.go` (manually validated). Silently counted in the summary only.
- `EXTRA_NEW`  — same as EXTRA but the machine type is **not** in `knownExtras`. This needs investigation: validate the price in the GCP Console, then add the machine to `knownExtras` and to the Known EXTRA entries table below.
- `UNAVAIL`    — a reference source lists a price but the machine type is not deployed in the region according to the Compute Engine `machineTypes` API. Silently counted in the summary only.
- `BLACKLIST`  — the machine type is deployed in the region but intentionally excluded from pricing by our blacklist (see `pkg/providers/pricing/instanceprice/`). Silently counted in the summary only.
- Exit code is `1` when any MISMATCH, MISSING, or EXTRA_NEW is found. EXTRA, UNAVAIL, and BLACKLIST do not affect the exit code.

---

## Known EXTRA entries

EXTRA means we compute a price but neither Cyclenerd nor GCP web has one. These are
legitimate machine types whose prices cannot be cross-validated automatically.

| Machine type | Regions | Notes |
|---|---|---|
| `a3-edgegpu-8g` | 14 | A3 Edge — 8× NVIDIA H100 for serving. Documented but not on standard pricing page. Ref: https://cloud.google.com/compute/docs/gpus |
| `a3-edgegpu-8g-nolssd` | 14 | Variant without local SSD. Same VM-level price as base — see note below. |
| `a3-megagpu-8g` | 2 extra | Priced correctly via "Plus" GPU SKU; 2 regions (asia-east1, europe-north1) not yet in reference sources. |
| `a3-ultragpu-8g-nolssd` | 7 | Variant without local SSD. Base price differs by ~$1.3/hr (local SSD component) — see note below. |
| `g4-standard-6/12/24` | 3 | New G4 family. GCP Compute API returns these and billing SKUs exist, but neither Cyclenerd nor GCP web pricing pages list them yet. GCP Console shows "A cost estimate for this machine type is not available right now" — cannot validate until reference sources catch up. |

### `-nolssd` variant pricing

The Compute API returns both `a3-*-8g` and `a3-*-8g-nolssd` variants. The
`-nolssd` variant has no local SSD in its machine spec, so the local SSD billing
component is $0. This means:

- `a3-edgegpu-8g` = `a3-edgegpu-8g-nolssd` ($87.83) — base has no SSD in the
  Compute API `scratchDisks` field either, so both are identical.
- `a3-ultragpu-8g` ($84.81) > `a3-ultragpu-8g-nolssd` ($83.49) — base includes
  local SSD at ~$1.3/hr (varies by region).

### `a3-ultragpu-8g` $0 GPU SKU in `europe-west4` and `us-south1`

In these 2 regions the Billing Catalog has no standard on-demand H200 GPU SKU —
only DWS, Reserved, and Commitment variants. The computed price reflects only
CPU + RAM + SSD (~$10–12/hr instead of ~$84). The GCP Console confirms this:
`8 NVIDIA H200 141GB = $0.00` in the billing breakdown, and in `europe-west4`
the base `a3-ultragpu-8g` is listed as **Reservation-bound** (not available for
standard on-demand provisioning).

Our computed prices match the Console exactly ($10.91/hr for base, $9.46/hr for
`-nolssd` in `europe-west4`). This is correct billing behavior, not a bug.

### Manual validation results (GCP Console, 2026-03-25)

| Machine type | Region | Console $/hr | Computed $/hr | Diff | Status |
|---|---|---|---|---|---|
| `a3-edgegpu-8g` | us-central1 | $88.49 | $87.83 | −0.74% | ✅ (SSD component underpriced by ~$0.66/hr) |
| `a3-edgegpu-8g-nolssd` | us-central1 | $87.83 | $87.83 | 0.00% | ✅ |
| `a3-ultragpu-8g` | us-central1 | $84.81 | $84.81 | 0.00% | ✅ |
| `a3-ultragpu-8g-nolssd` | us-central1 | $83.49 | $83.49 | 0.00% | ✅ |
| `a3-ultragpu-8g` | europe-west4 | $10.91 | $10.90 | −0.09% | ✅ ($0 GPU — reservation-bound region) |
| `a3-ultragpu-8g-nolssd` | europe-west4 | $9.46 | $9.46 | 0.00% | ✅ ($0 GPU — see note above) |

---

## Machine type blacklist

Some machine types returned by the Compute API are excluded from pricing because
their prices cannot be verified or are known to be incorrect. Blacklisted machines
produce no offerings in Karpenter — they are invisible to the scheduler, not treated
as free.

The blacklist is currently empty. It will be populated when the SKU-based pricing
rewrite (`instanceprice` internals) ships in a follow-up PR.

| Entry | Reason |
|---|---|
| _(none yet)_ | — |

---

## Maintaining SKU family matchers

> **Note:** This section applies to the upcoming SKU-based pricing rewrite. The
> current implementation uses the Cyclenerd community CSV and does not have
> configurable family matchers.

When the SKU-based `instanceprice` rewrite ships, the pricing engine will match
billing SKU descriptions to machine families. When GCP launches a new machine
family (e.g. C5, N5), the following updates will be needed:

1. Add the family to the family matchers with the correct CPU/RAM SKU description
   prefixes. Find these by searching the Billing Catalog for the new family name.
2. If the family has family-specific local SSD pricing, add it to the local SSD
   family SKU prefixes.
3. If the family has local SSD capacity overrides (DiskGb=0 in the API), add
   the appropriate overrides.
4. Run `price_validate` to verify the new family's prices match reference sources.

Machines whose family is unrecognized will have no computed price and will not
appear as Karpenter offerings. The `MISSING` classification in `price_validate`
output detects this — run validation after every GCP machine family announcement.

