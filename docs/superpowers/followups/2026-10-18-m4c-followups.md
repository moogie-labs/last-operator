# M4c (image GC and external registry mode) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-18-m4c-gc-and-external-registry.md`.

## Result

**The full e2e passed** on the two-node kind cluster `last-m4a` after the final fix wave: `ok test/e2e 892.6s`, with every test passing.
- **`TestImageGCReclaimsUnusedImages`:** an unused image was deleted and its storage freed (`image.gc.result` `{deleted: 3, bytesFreed: 14521, kept: 0, deferred: 0}`, counting earlier runs' leftovers). A referenced image survived, and its manifest and blobs still answer through the front proxy. The deleted image is rebuilt when needed again.

**Bugs the reviews and the live cluster found:**
- **A blob cache that outlives GC.** Distribution's in-memory blob descriptor cache let the serving registry report collected blobs as present. BuildKit then skipped re-uploading them, producing images with missing layers. The cache is now off (`REGISTRY_STORAGE_CACHE_BLOBDESCRIPTOR: ""`), pinned by `helm-lint`.
- **A sidecar sweep that outlived the gateway.** A detached sidecar run kept sweeping after the gateway had resumed builds. The run now ends when the gateway disconnects, and the gateway's timeout (31 min) is longer than the sidecar's (30).
- **A port clash.** The registry's debug server took port 5001, the sidecar's port, and crash-looped the pod. It now listens on `127.0.0.1:5002`, pinned by `helm-lint`.
- **A restart on every upgrade.** A `checksum/auth` annotation would have restarted the single-replica registry on every upgrade. The admin htpasswd is now a refreshable directory mount instead.
- **Dotless registry hosts.** They would have sent the push credential to Docker Hub, and are now rejected, as is a bare `localhost`.
- **Untruthful GC records.** GC recorded deletions that hadn't happened after a bundled/external mode switch. Each index entry now records its registry.

## Before relying on external mode

- **Not tested against real registries.** ECR, GHCR and Harbor DELETE answers and token flows have only been tested against a TLS fake. Run one build, one run and one GC against each target before calling it supported.
- **Build pods get no pull Secret in external mode.** A private sandbox base kept in the external registry pulls in sandbox pods but not in build pods (main spec §11.3).
- **A rebuild can race a DELETE in external mode.** External-mode GC doesn't pause builds, so a rebuild of a just-dropped spec could race its DELETE.
  - **Why it's negligible:** harm needs an identical digest, and BuildKit's config `created` time makes that practically impossible.
  - **If it did happen,** the HEAD cache would hand out the dead digest for up to 10 minutes, then heal itself.
  - **A cheap guard:** after a DELETE that succeeded, if the index names the same digest again, call `forgetHead` and `dropImage`.
- **Pull-Secret rotation leaves a gap.** The copy is deleted, then re-created, and the pull grace covers the window. The copy also carries no labels.

## GC

- **A failed DELETE orphans its manifest,** because the index entry was dropped first. Expose this as a metric.
- **The pod's grace period and the sidecar's drain wait are separate constants:** a 1560 s grace period and a 25-minute wait. A wedged sidecar delays every registry recreate by up to 26 minutes.
- **Chart upgrades recreate the registry pod,** because the sidecar image follows the chart tag (documented). The outage is longer if a GC is running.
- **`kept` overcounts.** It also counts entries that left the index for other reasons, such as a failed verification or a replacement digest.
- **The sidecar's first size walk is unbounded.** It runs before the 30-minute bound starts, so very large storage can push a run past the gateway's 31 minutes. The disconnect then kills the run, which is safe, but loses the partial `bytesFreed`.
- **The plan's Global Constraints payload table is stale.** The main spec §11.3 has the final `image.gc.result` fields (`kept`, `deferred`).
- **Changing `LAST_REGISTRY_FRONT_HOST` orphans bundled storage,** because entries record an explicit registry.
- **An image rebuilt after a mode round-trip leaves its old manifest unindexed** (pre-existing).
- **No lower bound on `unusedRetention`.** The e2e uses 1 s.
- **Tests:**
  - no test that the first run waits a full interval;
  - `runImageGC` returns `ctx.Err` after a clean run.
- **The cluster-wide Intent and ManagedResource Lists are unpaginated.**

## Sidecar

- **No throttle on authentication.** Every unauthenticated request costs a bcrypt compare; the NetworkPolicy limits callers to the gateway.
- **`dirSize` walks the data directory twice per run.**

## Credentials

- **URL- or JSON-escaped credential forms are not redacted.** BuildKit doesn't print credentials that way.
- **Registry Secrets are forbidden to Providers only while `registry.external` names them** (documented).
