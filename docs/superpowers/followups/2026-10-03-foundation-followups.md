# Foundation (M0+M1) follow-ups

Deferred findings from the per-task and final whole-branch reviews of
`docs/superpowers/plans/2026-09-27-foundation-m0-m1.md` (commits
`b885df1..9d7e6fb`). Items already fixed by the final fix wave (F1–F8)
are not listed.

## Address in M2

These touch code that M2 builds on directly.

- **Persisted head witness.** Record the last written `(seq, hash)` outside
  the bucket (e.g. a gateway-owned ConfigMap or Lease annotation) and refuse
  `Open` when the store's head is behind it. This detects a rolled-back or
  restored store; today only `verify --expect-head` catches it.
- **Outcome payload numbers.** If `Do`'s `after` builds a payload containing a
  float or an integer above 2^53, `Do` returns "outcome not recorded" after the
  action ran, leaving a dangling before-entry. Settle a result-encoding
  convention (numbers as strings) or write a fallback outcome entry.
- **Draining in-flight `Do` on shutdown.** Process exit can drop an outcome
  record after the action ran. Drain before exit and set
  `terminationGracePeriodSeconds` to cover `PutAttempts × PutTimeout`.
- **Leader-election comment** (`internal/gateway/server.go`): the in-flight
  write window is `PutAttempts × PutTimeout + backoff` (~30s), longer than the
  15s lease. Fix the wording; the If-None-Match fence keeps it safe.
- **`otherChains` lists the whole `chains/` prefix** when the chain is empty.
  Stop at the first foreign key. Chain IDs containing `/` are misclassified.
- **Gateway config hardening:** required-variable errors are reported in map
  order (nondeterministic); whitespace-only values are accepted; the
  non-positive retention error ends in `<nil>`.
- **Writer callbacks run under `w.mu`.** Document that hooks must not call
  back into the Writer (the gateway's hooks comply).
- **Writer edge cases:** a `Get` failure after `ErrExists` is reported as
  `ErrChainConflict` instead of `ErrWriterBroken`; negative `PutAttempts`
  gives a confusing error; `sinceCheckpoint`/`lastCheckpoint` reset on `Open`
  (a crash-looping gateway may never checkpoint — seed from the verify report);
  a 409 `ConditionalRequestConflict` leads to a terminal conflict instead of a
  retry.
- **Gateway tests:** `Run` (leader election wiring, Cause propagation) is
  untested; no tests for a corrupt Secret, the ConfigMap create race, a
  non-NotFound ConfigMap Get error, or an empty-data ConfigMap on first install.
- **`ALLOW_NEW_CHAIN` override** is not exposed in the chart or README.

## Security hardening (before first release)

- **Auditor access:** the reader Secret sits next to `last-audit-root`, so the
  README's verify recipe needs `get secrets` in `last-audit`. Ship a Role scoped
  to `resourceNames: [last-audit-reader]` or move the reader Secret.
- **NetworkPolicy** admits every pod in `last-gateway`; add a `podSelector` for
  the gateway pod.
- **TLS** to RustFS (including the admin API that carries user secrets).
- **Workload identity / IRSA** for external S3 (the chart always injects static keys).
- **Pod hardening:** `automountServiceAccountToken: false` for RustFS and the
  init Job; Pod Security Admission `restricted` labels; RustFS `/logs` as a
  sized `emptyDir` with `readOnlyRootFilesystem: true`.
- **Supply chain:** pin `golang`, `distroless`, `rustfs/rustfs` and `kind` by
  digest/version; re-verify the provenance of the `azure/setup-helm` SHA.
- **`kubectl last audit log`** prints entries unverified and unsanitised; label
  the output and escape control characters.
- **HTTP-status fallback** in `hasCode` is broad (any 403 → `IsAccessDenied`,
  404 `NoSuchBucket` → `ErrNotFound`); the least-privilege integration test
  should match on error code only.

## Verifier hardening

- Require `o.Key == EntryKey(chainID, seq)` (nested keys with a valid basename pass).
- Check checkpoint index objects for delete markers.
- Report out-of-order/duplicate keys as their own problem (today a `gap` with
  an underflowed range).
- Turn a `Get` failure on a listed version into a Problem instead of aborting.
- `ProblemChainMismatch` has no regression test; the checkpoint-index `Detail`
  doesn't say which sub-condition failed; `FromSeq` pointing at a malformed
  entry reports "no entry at start seq".
- Optional `verify --check-retention` (GetObjectRetention per entry).

## Chart and tooling

- Bundled mode takes its region from `audit.external.region`; hard-code
  `us-east-1` when bundled.
- `config/manager/manager.yaml` still runs `/manager` (image now `/app`), so
  `make deploy` is broken — fix when M2 adds the operator Deployment.
- StatefulSet `serviceName` points at a non-headless Service; no checksum
  annotation rolls the gateway on Secret/ConfigMap change; only the gateway gets
  the `Version` ldflag; the Dockerfile dropped the scaffold's `BASE_IMAGE` override.
- CI drift check ignores untracked files, `zz_generated.deepcopy.go`, and
  `go mod tidy -diff`; `kind@latest` is unpinned; `on: [push, pull_request]`
  double-runs; the scaffold's `lint-config` step was dropped; golangci-lint has
  no build tags, so `integration`/`e2e` files are never linted.
- No real-store test that the gateway refuses an unlocked bucket (MemSink only).
- `S3Sink.List` can loop on a store that returns `IsTruncated` with
  non-advancing markers; `CreateBucket` sends no `LocationConstraint`.
- `AGENTS.md` (kubebuilder-generated) still describes `cmd/main.go` and Ginkgo tests.

## Documentation

- Spec §14.5 `verify` description doesn't mention `--allow-empty`, empty-chain
  failure, or the fingerprint line.
- README: a conformance run with admin credentials leaves two probe objects,
  not one; a recovery runbook for a permanently failing chain (a stray locked
  object after the last checkpoint bricks startup for the retention period —
  recovery is a new chain that links to the old head).
- Conformance: no unit test pins 403 `SignatureDoesNotMatch` → inconclusive;
  check order is not test-enforced.

## Minor code polish

- `conformance.go`: `const codeAccessDenied` sits between the `refusalCodes`
  doc comment and its `var`.
- goconst-extracted test constants conflate meanings (`nameDefault`,
  `testValue`, `modeCompliance`).
- `auditinit.waitReady`'s timeout error omits the last connection error;
  `AddUser` re-enables a manually disabled user on rerun.
- `MemSink.TamperAddDeleteMarker` silently no-ops on a missing key.
- Payload edge cases: `-0` is stored as `0`; `json.RawMessage(nil)` is now rejected.
