# M4b (image builds) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-17-m4b-image-builds.md`.

## Result

**The full e2e passed** on the two-node kind cluster `last-m4a`, after the final fix wave: `ok test/e2e 469s`. The registry check and every test passed, including the two image tests.

**The final review's fixes:**
- A build that fails during discovery now gives the model a "did not run" turn with up to 16 KiB of the build log.
- Sandbox pods start the runner copied from the pinned base, so a built image cannot replace it.
- A replan after a failed build gets the log's tail.
- The operator retries Unavailable on RunCommand.
- Build flights are cancelled, with their result recorded, when the gateway drains.
- Expired build objects are reaped every minute.

**Done-criterion tests:**
- `TestImageBuildIsReused` builds one image (one `image.build.start` for its spec hash across the whole chain). The command, its probe and a second Intent all run on that digest.
- `TestImageBuildFailureFailsTheStep` proves that a failed build fails the step and that the command never starts.

**Bugs only a live cluster showed:**
- **`AllowPrivilegeEscalation: false` breaks rootless BuildKit:** `no_new_privs` stops `newuidmap`. Caught in review by reproducing the failure against the pinned image.
- **The build pod read logs from the wrong container.** Caught in review.
- **kubelet rejects AppArmor `RuntimeDefault` on nodes without AppArmor.** Caught by the first e2e run. The init container now leaves AppArmor unset.

## Before the next real-model demo

- **Run `ANTHROPIC_API_KEY=… go run ./hack/schemaprobe`.** The turn schema grew from 2069 to 2157 bytes when `install` was added. Design §5 makes this rerun a gate, and it needs your key.

## Before M5 builds on this

- **The final status hides `ImageBuildFailed`.** This behaviour predates M4b and applies to any failed step.
  - **What happens:** after a step fails, the operator replans. When the model returns the plan that just failed, the gateway refuses it ("plan already opened a phase"), and the Intent's final message no longer shows the original cause.
  - **The fix belongs in the operator's plan acceptance** (`internal/controller/intent_planning.go`). An identical plan should get a correction turn ("identical to the plan that failed: <feedback>") with a bounded number of attempts. If those run out, the Intent should fail with the original feedback.
  - **The gateway's message also misleads.** "replan to run it again" is wrong after a replan.
- **Join audit for a shared failed build.** When Intent B joins Intent A's build flight and the build fails, B's audit trail has only the operator-written `phase.end`, while the `image.build.result` names A. Add a gateway-written entry for B that points at the result's sequence number.
- **Probe resolves are sequential.** Two uncached specs (2 × up to 35 minutes) can exceed `probeTimeout` (45 minutes). Later runs recover, because the first image is indexed by then. Resolve distinct specs in parallel, up to `maxConcurrentBuilds`.
- **A shared flight builds with the first caller's policy.** A narrow-policy first caller can fail a build that a wider-policy waiter needed. The audited `egress` field records which policy was used. Document this, or key flights on the effective build egress.

## Gateway

- **Push builds to the front Service's ClusterIP, not its DNS name.** With NodeLocal DNSCache (169.254.20.10), every build's lookup fails, because build egress allows only kube-dns pods.
- **`maxConcurrentBuilds` is applied only when leadership starts,** while `buildTimeout` is read for every build. Resize it when OperatorConfig is read.
- **Pin the BuildKit image by digest**, not by tag (values and the gateway default).
- **Disabled bundled registry with no external mode:** builds hit Forbidden on the registry Role and return Unavailable. They should fail with a clear FailedPrecondition.
- **Index (`internal/gateway/imageindex.go`):**
  - Kubernetes I/O runs while the index lock is held.
  - The hourly flush gate compares against the persisted `lastUsed`, not the time of the last write.
  - The in-memory index and the ConfigMap can diverge after a failed save. This is safe, because every read goes through `verifyBuild`.
  - The index grows without bound until M4c's garbage collection.
  - Payload decoding is lenient, though the payload is signed.
  - There is no chain ID check (the sink key already scopes it).
- **Resolver:**
  - `resize` briefly exceeds the cap.
  - `RegistryNamespace` and the front host are not checked against each other.
  - OperatorConfig is read twice per resolve.
- **Build-failed step result:** it is not recorded in the phase's results, and its `RunId` is empty (documented as "no run started").

## From the final re-review

- **A refusal on a retry looks like the earlier Unavailable.** A FailedPrecondition that first appears when the operator retries an Unavailable RunCommand is reported as the earlier Unavailable error.
  - **In discovery,** the model is told "may have run".
  - **In execution,** the Intent's failure names the wrong cause.

  The outcome is the same, and the gateway's `policy.denied` entry has the real reason.
- **The reaper runs on the leader's loop.** `reapExpiredBuilds` runs synchronously there, so a slow API server delays the index-flush tick.
- **Main spec §11.3 still says pushed tags "cannot affect what runs".** Align it with the tightened §17.2 wording.
- **Probes read the sandbox base twice:** once for the runner and once per group in `resolveImage`. A config change between the two reads could pair a runner copy with an image built on another base. The commands path reads it once.

## Build pods

- **Validate `FrontHost` and the build ID.** Both reach a shell script and labels; today they come only from admin config.
- **Pod-cannot-start and wait-limit count as build failures.** A missing BuildKit image therefore surfaces as `ImageBuildFailed`, which the model cannot fix by replanning.

## Tests

- **`internal/images`:**
  - BaseDigest edge cases: uppercase hex, a 65-character digest, multiple `@`.
  - The CEL 30m `buildTimeout` rule.
  - Malformed reaper expiry labels.
  - A few untested builder branches.
- **Gateway:**
  - A cancelled flight still calls `inflight.Done` on every path.
  - A probe FailedPrecondition.
  - The corrupt-index case with a bad `lastUsed`.
  - A concurrent `-race` test of the index.
- **Agent:**
  - The round trip of `install` in probes and teardown.
  - The prompt test does not pin update-before-install order.
- **e2e:**
  - An OperatorConfig the test created is not deleted.
  - Namespace deletion is not awaited.
  - `runs >= 4` is only a lower bound.

## Carried from M4a

The M4a follow-ups (`docs/superpowers/followups/2026-10-17-m4a-followups.md`) still apply: EKS and GKE validation, the `--from` test, configurable names in the registry check, NetworkPolicy enforcement in the check, and the `last-sandbox` Pod Security label.
