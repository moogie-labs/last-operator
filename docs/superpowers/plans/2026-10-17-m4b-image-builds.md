# M4b: Image Builds Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a command whose `image.install` needs tools outside the base image run end to end. The gateway builds the image once with rootless BuildKit, records the build in the audit log, and runs the command, its probes, and every later command with the same spec on that digest.

**Architecture:**
- **`internal/images`** gains the pure parts:
  - spec hashing;
  - Containerfile generation;
  - the build pod's objects;
  - a `Builder` that runs one build pod in `last-build` and reads back the digest.

  The build pod runs our `sandbox-runner` as its entrypoint, so builds get the same egress gate and result envelope as sandbox pods.
- **The gateway** gains:
  - an image resolver, with an index persisted in a ConfigMap and each entry checked against its signed `image.build.result`;
  - single flight per spec hash, and a cap on concurrent builds.

  `RunCommand` and `RunProbes` resolve images before running.
- **The operator's agent** learns to emit `image.install`. The CLI shows the install commands, and the operator's deadlines grow by the maximum build time.

**Tech Stack:** Go 1.26, client-go and controller-runtime, go-containerregistry v0.20.3 (M4a's `internal/images.Client`), BuildKit `moby/buildkit:v0.26.2-rootless`, Helm, kind.

**Spec:** `docs/superpowers/specs/2026-10-17-m4-image-builds-design.md`:
- §4 and §5, plus §7 and §8;
- §9 (the M4b parts) and §10 (the amendments to main spec §5.6, §6, §11.2 and §17.2);
- decisions D2 and D4 through D9.

Evidence for the build pod's configuration is in `docs/superpowers/spikes/2026-10-17-m4-registry-spike.md` (finding 6). M4a, the bundled registry, is done; its follow-ups are in `docs/superpowers/followups/2026-10-17-m4a-followups.md`. The parent spec is `docs/superpowers/specs/2026-09-26-last-operator-design.md`.

## Global Constraints

- **Commits and files:** never add a commit attribution trailer (such as `Co-authored-by:`). Never commit `.superpowers/`. Never edit generated files by hand; after changing `api/` types, run `make manifests generate helm-crds`.
- **Credentials:** a registry password, and the base64 `auth` string built from it, never appears in logs, errors, audit payloads or command lines. Build output is redacted (raw and base64) before it is audited or returned.
- **Spec hash:** `specHash = "sha256:" + hex(sha256(audit.Canonical({"baseDigest": <digest>, "install": [{"argv": [...]}...]})))`. `baseDigest` is the `sha256:<64 hex>` part of the pinned base reference.
- **Containerfile:** exactly main spec §11.2's form:
  - `FROM <base>`;
  - `USER root`;
  - one `RUN <json argv>` per install command (exec form; JSON written without HTML escaping);
  - `USER 65532`.

  Never `COPY`, `ARG` or heredocs.
- **References** (bundled mode only; external mode arrives in M4c):

  | Use | Reference |
  |---|---|
  | push | `<frontHost>/last/images:sha256-<hex>` |
  | run | `<pullHost>/last/images@<digest>` |
  | build `FROM` | the base with `<pullHost>/` rewritten to `<frontHost>/` |

  The defaults are `frontHost = last-registry-front.last-registry.svc:5000` and `pullHost = localhost:5000`.
- **Build pod** (spike finding 6, design §4.2):
  - namespace `last-build`, image `moby/buildkit:v0.26.2-rootless`, UID and GID 1000;
  - seccomp and AppArmor `Unconfined`;
  - `BUILDKITD_FLAGS=--oci-worker-no-process-sandbox --config /work/buildkitd.toml`;
  - `automountServiceAccountToken: false`;
  - limits: CPU 2, memory 4Gi, ephemeral storage 10Gi;
  - `activeDeadlineSeconds` = `buildTimeout` + 60 s.
- **Build egress:**
  - the registry (front Service ClusterIP and endpoints);
  - kube-dns on UDP and TCP 53;
  - `images.buildEgress`, keeping only the entries the requesting Intent's policy allows (`Effective.AllowsEgress`).

  Hostname entries are refused (`FailedPrecondition`). A grant that covers an egress canary is refused.
- **Config defaults** (`OperatorConfig.spec.images`):
  - `buildTimeout`: 15m, at most 30m;
  - `maxConcurrentBuilds`: 2, from 1 to 8;
  - `buildEgress`: empty.
- **Audit entries.** Both carry the requesting Intent, stage and subject hash.

  | Entry | Payload fields |
  |---|---|
  | `image.build.start` | `specHash`, `baseImage`, `containerfile`, `pushRef` |
  | `image.build.result` | `specHash`, `startSeq`, `outcome` (`succeeded` or `failed`), `digest`, `durationMs`, `log`, `logTruncated`, `error` |
- **Index:**
  - the ConfigMap `last-image-index` in the gateway namespace, key `index.json`;
  - a map from spec hash to `{"digest", "buildSeq", "lastUsed"}`;
  - `lastUsed` in RFC 3339 is updated in memory and flushed at most once an hour per entry;
  - a successful manifest HEAD is cached for 10 minutes.
- **Failure:**
  - a failed or timed-out build fails the command with `Error: "ImageBuildFailed: <reason>"`, `ExitCode: -1`, and the last 64 KiB of the build log as `Stderr`;
  - a base that isn't pinned by digest is `FailedPrecondition`: "image builds need OperatorConfig.sandbox.baseImage pinned by digest";
  - registry unreachable is `Unavailable`.
- **Operator deadlines:** `stepTimeout` 60m (was 30m); `probeTimeout` 45m (was 15m).
- **After Go changes:** run `make lint-fix`, `make lint` (0 issues) and `make test`. **After chart changes:** run `make helm-lint` and `make nginx-check`.

## Review Focus

1. **Two Intents, or two probe groups, need the same uncached spec at once.** Exactly one build must run, and both must get its digest. Task 5 tests concurrent resolves with a build fake that counts calls.
2. **The index points at a digest its audit entry doesn't name** (an edited ConfigMap, a mismatched spec hash, or a failed outcome). That digest must never run; the gateway rebuilds instead. Task 2 tests each case.
3. **The manifest disappeared from the registry behind the index.** The resolver must rebuild, not hand kubelet a dead digest. Task 5 tests it with the in-memory registry.
4. **An install argv contains quotes, backslashes, newlines or non-ASCII.** The Containerfile must reproduce it exactly. Task 1 parses the generated `RUN` line back and compares.
5. **The gateway restarts mid-build.** The leftover build pod, Secret and NetworkPolicy must be reaped, and the next request must rebuild. Task 4 tests the reaper by label; Task 5 tests that a missing index entry rebuilds.

---

### Task 1: Images API, spec hash and Containerfile

**Files:**
- Modify: `api/v1alpha1/operatorconfig_types.go` (`ImagesConfig`)
- Create: `internal/images/spec.go`, `internal/images/spec_test.go`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §5.6 and §6:
  - `buildEgress` takes IP and CIDR entries only, and defaults to empty;
  - the new fields;
  - delete the hostname list from the §5.6 example, and change the defaults table row.
- Regenerate: `make manifests generate helm-crds`

**Interfaces:**
- Produces:

  ```go
  // api/v1alpha1: ImagesConfig gains
  //   BuildTimeout *metav1.Duration `json:"buildTimeout,omitempty"`        // default 15m, CEL: <= 30m
  //   MaxConcurrentBuilds int32 `json:"maxConcurrentBuilds,omitempty"`    // +kubebuilder:default=2, Minimum=1, Maximum=8

  package images
  // BaseDigest returns the "sha256:<64 hex>" digest of a reference pinned by digest, or an error.
  func BaseDigest(ref string) (string, error)
  // SpecHash returns "sha256:<hex>" of the canonical {baseDigest, install} (Global Constraints).
  func SpecHash(baseDigest string, install []lastv1alpha1.ArgvCommand) (string, error)
  // Tag is the push tag for a spec hash: "sha256-<hex>".
  func Tag(specHash string) string
  // Containerfile renders main spec §11.2's Containerfile for base (a full reference).
  func Containerfile(base string, install []lastv1alpha1.ArgvCommand) (string, error)
  // DefaultBuildTimeout = 15 * time.Minute; MaxBuildTimeout = 30 * time.Minute; DefaultMaxConcurrentBuilds = 2
  ```

- [ ] **Step 1: Write the failing tests** (`internal/images/spec_test.go`)

  - `TestSpecHashIsTheHashOfTheCanonicalSpec`:
    - base digest `"sha256:" + strings.Repeat("a", 64)`, install `[{Argv: ["apt-get","install","-y","awscli"]}]`;
    - the expected value is `"sha256:" + hex(sha256([]byte(`{"baseDigest":"sha256:aaa…a","install":[{"argv":["apt-get","install","-y","awscli"]}]}`)))`, built from that literal string in the test.
  - `TestSpecHashChangesWithTheBaseAndEveryArgv`: changing the base digest, one argv element, or the install order each changes the hash.
  - `TestBaseDigestNeedsADigest`:
    - `localhost:5000/last/base@sha256:<64 hex>` gives the digest;
    - `ghcr.io/x/y:e2e` and `x@sha256:short` give errors.
  - `TestContainerfileHasTheSpecForm`: for base `B` and two installs, the output is exactly `"FROM B\nUSER root\nRUN [\"a\",\"b\"]\nRUN [\"c\"]\nUSER 65532\n"`.
  - `TestContainerfileKeepsAwkwardArgvExact` (Review Focus 4): with argv `["sh","-c","echo \"x\" \\ $HOME\nnext","<&>","é"]`, the `RUN` line's JSON after `RUN ` unmarshals to the same slice, and the line holds a literal `<&>` (no HTML escaping).
  - `TestContainerfileRefusesEmptyInstall`: empty install, or an empty argv, gives an error.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/images/ -run 'Spec|Base|Containerfile'`. Expected: FAIL to compile.

- [ ] **Step 3: Implement**

  - **The API fields:**
    - add the markers in Interfaces;
    - add the CEL rule on `ImagesConfig`: `!has(self.buildTimeout) || duration(self.buildTimeout) <= duration('30m')`, with the message "buildTimeout must be at most 30m";
    - run `make manifests generate helm-crds`.
  - **`spec.go`:** use `audit.Canonical` for the hash. For each `RUN` line, use a `json.Encoder` with `SetEscapeHTML(false)` and trim its newline.
  - **The spec edits** listed under Files.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/images/ ./api/... && make lint && make helm-lint`. Expected: PASS, 0 issues.

- [ ] **Step 5: Commit**

  ```bash
  git add api internal/images config charts docs/superpowers/specs/2026-09-26-last-operator-design.md
  git commit -m "feat(images): hash image specs and render their Containerfiles"
  ```

---

### Task 2: The persisted, audit-checked digest index

**Files:**
- Create: `internal/gateway/imageindex.go`, `internal/gateway/imageindex_test.go`

**Interfaces:**
- Consumes: `audit.DecodeEntry`, `audit.CheckSeal`, `audit.EntryKey`, `audit.TypeImageBuildResult`; the gateway's `sink`, `signer` and `cfg.ChainID` (as `readIndexed` in `internal/gateway/queryaudit.go` uses them); the Kubernetes client for ConfigMaps in the gateway namespace.
- Produces:

  ```go
  type imageEntry struct {
      Digest   string    `json:"digest"`
      BuildSeq uint64    `json:"buildSeq"`
      LastUsed time.Time `json:"lastUsed"`
  }
  // buildResult is the image.build.result payload (Global Constraints); Task 5 writes it.
  type buildResult struct {
      SpecHash     string `json:"specHash"`
      StartSeq     uint64 `json:"startSeq"`
      Outcome      string `json:"outcome"` // "succeeded" | "failed"
      Digest       string `json:"digest,omitempty"`
      DurationMs   int64  `json:"durationMs"`
      Log          string `json:"log"`
      LogTruncated bool   `json:"logTruncated"`
      Error        string `json:"error,omitempty"`
  }
  type imageIndex struct { /* kube, namespace, mu, entries map[string]imageEntry, verified map[string]bool, flushed map[string]time.Time */ }
  func newImageIndex(kube kubernetes.Interface, namespace string) *imageIndex
  func (x *imageIndex) load(ctx context.Context) error                          // reads ConfigMap last-image-index; a missing one is empty
  func (x *imageIndex) get(specHash string) (imageEntry, bool)
  func (x *imageIndex) put(ctx context.Context, specHash string, e imageEntry) error // saves at once (create or update)
  func (x *imageIndex) drop(ctx context.Context, specHash string) error
  func (x *imageIndex) touch(specHash string, now time.Time)                    // lastUsed in memory only
  func (x *imageIndex) flushUsed(ctx context.Context, now time.Time) error      // saves when an entry's lastUsed changed and its last flush is over an hour old
  // verifyBuild reads the signed entry at e.BuildSeq and checks it is the image.build.result
  // that produced e.Digest for specHash with outcome "succeeded". A pass is cached per process.
  func (g *Gateway) verifyBuild(ctx context.Context, specHash string, e imageEntry) error
  ```

- [ ] **Step 1: Write the failing tests**

  Use a fake clientset and the audit test sink and signer that `queryaudit_test.go` uses.
  - `TestImageIndexRoundTrips`: `put` and then a fresh `load` returns the entry. A missing ConfigMap loads as empty.
  - `TestImageIndexFlushesLastUsedAtMostHourly`: two `touch`+`flushUsed` calls within an hour cause one ConfigMap update; a call after an hour causes another (count update actions on the fake).
  - `TestVerifyBuildAcceptsTheEntryThatBuiltTheDigest`: write a sealed `image.build.result` entry with `outcome: succeeded` through the writer, and check that `verifyBuild` with its seq and digest returns nil.
  - `TestVerifyBuildRejects` (Review Focus 2), with subtests, each returning an error:
    - a different digest;
    - a different spec hash;
    - outcome `failed`;
    - an entry of another type at that seq;
    - a missing seq;
    - an entry whose signature fails (re-sealed with another key).

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/gateway/ -run 'ImageIndex|VerifyBuild'`. Expected: FAIL to compile.

- [ ] **Step 3: Implement `imageindex.go`**

  The ConfigMap holds `index.json` as a JSON object keyed by spec hash. Saves use create-or-update with the resourceVersion read at `load`, so a conflict is retried once after a reload. `verifyBuild` follows `readIndexed`'s decode and seal checks, then decodes the payload into `buildResult`.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/gateway/ -run 'ImageIndex|VerifyBuild' && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/gateway/imageindex.go internal/gateway/imageindex_test.go
  git commit -m "feat(gateway): keep the image digest index in a ConfigMap, checked against the audit log"
  ```

---

### Task 3: Build pod objects

**Files:**
- Create: `internal/images/buildpod.go`, `internal/images/buildpod_test.go`
- Modify: `internal/sandbox/egress.go` (add `ServiceEgress`, generalising `APIServerEgress`)
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §17.2. Add the design §8 bullet: install steps can read the pusher credential, which lets them push arbitrary tags (storage only, never what runs) but never delete.

**Interfaces:**
- Consumes: `images.Containerfile`, `images.Tag` (Task 1); `runnerapi.Spec`, `runnerapi.Command`, `runnerapi.SpecPath`; `sandbox.EgressRule`, `sandbox.EgressPolicy` shapes; `sandbox.LabelManagedBy`, `ManagedByValue`, `LabelExpiresAt`.
- Produces:

  ```go
  // sandbox.ServiceEgress returns rules for a Service's ClusterIP and its EndpointSlice
  // addresses on each port (APIServerEgress now calls it for default/kubernetes).
  func ServiceEgress(ctx context.Context, kube kubernetes.Interface, namespace, name string) ([]EgressRule, error)

  package images
  const LabelBuild = "last.moogie-labs.com/build"
  type BuildPodConfig struct {
      Namespace, BuildKitImage, RunnerImage string // RunnerImage: the pinned base, which holds /last/sandbox-runner
      FrontHost string
      Timeout   time.Duration
      Canaries  []string // passed to the runner's egress gate
  }
  type BuildSpec struct {
      ID, SpecHash, Base string // Base: the pinned base reference
      Install                 []lastv1alpha1.ArgvCommand
      PushUser, PushPassword  string
      Egress                  []sandbox.EgressRule // registry + allowed buildEgress; DNS is added by the builder
  }
  // BuildObjects returns the spec Secret, the NetworkPolicy and the pod for one build.
  func BuildObjects(cfg BuildPodConfig, b BuildSpec, expires time.Time) (*corev1.Secret, *networkingv1.NetworkPolicy, *corev1.Pod, error)
  ```

  The objects are named `last-build-<ID>` (the Secret is `last-build-<ID>-spec`). All carry `LabelManagedBy: last-gateway`, `LabelBuild: <ID>`, `app.kubernetes.io/name: last-build` and the expiry label.

- [ ] **Step 1: Write the failing tests**

  - `TestBuildObjectsRunTheRunnerUnderBuildKit`:
    - the pod has one init container (image `RunnerImage`, `["cp","/last/sandbox-runner","/last-bin/sandbox-runner"]`, UID 1000, emptyDir `last-bin`);
    - the main container has image `BuildKitImage` and command `["/last-bin/sandbox-runner"]`;
    - env includes `BUILDKITD_FLAGS=--oci-worker-no-process-sandbox --config /work/buildkitd.toml`, `DOCKER_CONFIG=/work/.docker` and `HOME=/home/user`;
    - the securityContext has UID/GID 1000 and seccomp and AppArmor `Unconfined`;
    - `automountServiceAccountToken` is false;
    - limits are CPU 2, memory 4Gi, ephemeral 10Gi;
    - `activeDeadlineSeconds` is `Timeout` + 60 s;
    - volumes: the spec Secret at `/last/spec`, emptyDirs at `/work`, `/last-bin` and `/home/user/.local/share/buildkit`.
  - `TestBuildObjectsCarryTheBuildInTheRunnerSpec`. Decode the Secret's `spec.json` with `runnerapi.DecodeSpec` and check:
    - one script command whose script runs `buildctl-daemonless.sh build --frontend dockerfile.v0 --local context=/work/ctx --local dockerfile=/work/ctx --output type=image,name=<FrontHost>/last/images:<Tag>,push=true,registry.insecure=true --metadata-file /work/meta.json` with its stdout sent to stderr, and then `cat /work/meta.json`;
    - `TimeoutSeconds` equal to `Timeout`;
    - files `/work/ctx/Containerfile` (equal to `Containerfile(rewrittenBase, install)`), `/work/buildkitd.toml` (with `[registry."<FrontHost>"]` and `http = true`) and `/work/.docker/config.json` (`auths.<FrontHost>.auth` = base64 of `user:password`);
    - `EgressCanaries` equal to `Canaries`.
  - `TestBuildObjectsRewriteABundledBase`: base `localhost:5000/last/base@sha256:…` becomes `FROM last-registry-front.last-registry.svc:5000/last/base@sha256:…`. Any other base is used unchanged.
  - `TestBuildPolicyAllowsOnlyTheGrantsAndDNS`:
    - the NetworkPolicy selects `LabelBuild: <ID>` and is egress-only;
    - its rules are exactly the given `Egress` (TCP), plus one rule to `namespaceSelector kubernetes.io/metadata.name=kube-system` with `podSelector k8s-app=kube-dns` on UDP and TCP 53.
  - `TestServiceEgressCoversClusterIPAndEndpoints`, in `internal/sandbox`, against a fake Service and EndpointSlice. The existing `APIServerEgress` tests must still pass.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/images/ ./internal/sandbox/ -run 'BuildObjects|BuildPolicy|ServiceEgress'`. Expected: FAIL.

- [ ] **Step 3: Implement**

  `buildpod.go` builds the objects. Refactor `APIServerEgress` to call `ServiceEgress(ctx, kube, "default", "kubernetes")`. Add the §17.2 bullet.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/images/ ./internal/sandbox/ && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/images internal/sandbox/egress.go internal/sandbox/egress_test.go docs/superpowers/specs/2026-09-26-last-operator-design.md
  git commit -m "feat(images): build pods run BuildKit behind the sandbox runner's egress gate"
  ```

---

### Task 4: The builder

**Files:**
- Create: `internal/images/builder.go`, `internal/images/builder_test.go`
- Modify: `internal/sandbox/executor.go`. Extract the pod wait loop from `await` (including the pull grace) into `WaitForPod`, and make `await` call it, so behaviour is unchanged.

**Interfaces:**
- Consumes: `BuildObjects`, `BuildPodConfig`, `BuildSpec` (Task 3); `images.Client.Head` (M4a); `runnerapi.ParseEnvelope`; `sandbox.KubeLogReader`'s type `sandbox.LogReader`.
- Produces:

  ```go
  // sandbox.WaitForPod polls until the pod finishes, fails fast, or limit passes (await's
  // current semantics, PullGrace included) and returns the finished pod.
  func WaitForPod(ctx context.Context, kube kubernetes.Interface, namespace, name string, limit, poll, pullGrace time.Duration) (*corev1.Pod, error)

  package images
  type BuildOutcome struct {
      Digest       string        // "sha256:…", set on success
      Log          string        // the build's stderr, redacted, at most runnerapi.MaxOutput
      LogTruncated bool
      Duration     time.Duration
      Err          error         // build failure (non-zero exit, timeout, no digest); nil on success
  }
  type Builder struct { /* kube, logs sandbox.LogReader, registry *Client, cfg BuildPodConfig, poll time.Duration, now func() time.Time */ }
  func NewBuilder(kube kubernetes.Interface, logs sandbox.LogReader, registry *Client, cfg BuildPodConfig) *Builder
  // Build creates the objects, waits for the pod, reads the runner envelope and confirms the
  // digest with Head; it always deletes the build's objects. A returned error means the build
  // could not be attempted or its outcome is unknown (Kubernetes or registry errors);
  // BuildOutcome.Err means the build itself failed.
  func (b *Builder) Build(ctx context.Context, s BuildSpec) (BuildOutcome, error)
  // ReapOrphans deletes every object labelled LabelBuild in the namespace whose expiry passed
  // (or all of them when all is true, as at startup).
  func (b *Builder) ReapOrphans(ctx context.Context, all bool) (int, error)
  ```

- [ ] **Step 1: Write the failing tests**

  Use a fake clientset whose pod reactor sets the phase, a fake `LogReader` returning a runner envelope, and the in-memory registry from `internal/images/registry_test.go` holding a pushed `random.Image`.
  - `TestBuildReturnsTheDigestFromTheMetadata`:
    - the envelope's stdout is `{"containerimage.digest":"<pushed digest>"}` and its stderr is the build log;
    - the outcome has that digest and that log;
    - afterwards no Secret, NetworkPolicy or pod with `LabelBuild` remains.
  - `TestBuildFailsWhenTheDigestIsNotInTheRegistry`: stdout names an unpushed digest, so `Err` is set and the error is nil.
  - `TestBuildFailsOnANonZeroExit`: exit code 1. `Err` says "build exited 1", and `Log` is the stderr.
  - `TestBuildFailsOnMalformedMetadata`: the stdout is not JSON, or has no digest, or a digest that doesn't match `^sha256:[0-9a-f]{64}$`. Each sets `Err`.
  - `TestBuildRedactsThePushCredential`: the stderr contains the password, base64 of `user:password`, and base64 of the password. All three become `[REDACTED]` in `Log`.
  - `TestReapOrphansDeletesExpiredBuildObjects` (Review Focus 5): expired objects with `LabelBuild` are deleted, unexpired ones are kept, and with `all` every one is deleted. Objects without the label are untouched.
  - The existing `internal/sandbox` executor tests must still pass, unchanged, after the `WaitForPod` extraction.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/images/ -run 'Build|Reap'`. Expected: FAIL.

- [ ] **Step 3: Implement**

  - `Build` follows the executor's launch order: create the Secret, then the NetworkPolicy, then the pod. The wait limit is `cfg.Timeout + 2m`, with a 60 s pull grace.
  - Cleanup runs with `context.WithoutCancel` and a 30 s timeout. It deletes by the `LabelBuild=<ID>` selector, as `deleteRun` does.
  - Redact with the same raw-plus-base64 approach as `newRedactor`.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/images/ ./internal/sandbox/ && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/images internal/sandbox/executor.go
  git commit -m "feat(images): run one build pod and read back the pushed digest"
  ```

---

### Task 5: The gateway's image resolver

**Files:**
- Create: `internal/gateway/images.go`, `internal/gateway/images_test.go`
- Modify: `internal/gateway/config.go`. Add these, with defaults:
  - `LAST_REGISTRY_FRONT_HOST` (`last-registry-front.last-registry.svc:5000`);
  - `LAST_REGISTRY_PULL_HOST` (`localhost:5000`);
  - `LAST_BUILD_NAMESPACE` (`last-build`);
  - `LAST_BUILDKIT_IMAGE` (`moby/buildkit:v0.26.2-rootless`).
- Modify: `internal/gateway/server.go` and `runtime.go`. Construct the index and resolver, `load` the index and reap all build objects in `prepareLeading`, and call `flushUsed` alongside `flushIndexes`.
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §11.2:
  - step 1 becomes design D2's persisted, audit-checked index;
  - build pods run in `last-build` (D3), without the process sandbox (D4), with DNS;
  - builds happen inside `RunCommand` and `RunProbes` (D7);
  - failure handling follows design §4.3.

**Interfaces:**
- Consumes: Tasks 1–4; `g.operatorConfig`, `g.sandboxImage`, `g.effectivePolicy` and `policy.Effective.AllowsEgress`; `sandbox.ParseEgress`, `sandbox.ServiceEgress`; `audit.Writer.Append`; the Secrets `last-registry-pusher` and `last-registry-admin` in the gateway namespace (keys `username`, `password`).
- Produces:

  ```go
  // imageRequest names who needs an image, for the build's audit entries.
  type imageRequest struct {
      Intent, Stage, Subject, Actor string
      Install                       []lastv1alpha1.ArgvCommand
      Policy                        policy.Effective
  }
  // errImageBuildFailed carries a failed build's reason and its log tail (the last 64 KiB).
  type errImageBuildFailed struct{ Reason, LogTail string }
  // resolveImage returns the reference a command runs on: the base when install is empty,
  // else "<pullHost>/last/images@<digest>", building it (single flight, capped) when needed.
  // gRPC errors: FailedPrecondition (unpinned base, external mode, hostname in buildEgress,
  // a grant covering a canary), Unavailable (registry or Kubernetes errors);
  // errImageBuildFailed for a failed build.
  func (g *Gateway) resolveImage(ctx context.Context, w *audit.Writer, req imageRequest) (string, error)
  ```

- [ ] **Step 1: Write the failing tests**

  Use the in-memory registry, a fake builder (an interface `imageBuilder { Build(ctx, images.BuildSpec) (images.BuildOutcome, error); ReapOrphans(ctx, all bool) (int, error) }` held by the gateway, so tests can count calls and block), and the gateway test env from `testenv_test.go`.
  - `TestResolveReturnsTheBaseWithoutInstall`.
  - `TestResolveBuildsOnceAndAuditsTheBuild`:
    - the first call builds;
    - `image.build.start` and then `image.build.result` (`outcome: succeeded`, the digest) are written, carrying the request's intent, stage and subject;
    - the result is `localhost:5000/last/images@<digest>`;
    - the index holds the digest and the result entry's seq;
    - a second call does not build.
  - `TestConcurrentResolvesShareOneBuild` (Review Focus 1): 5 goroutines, one spec, a builder blocked until released. Exactly 1 build happens, and all 5 get the same reference.
  - `TestAFailedBuildFailsEveryWaiterAndIsNotIndexed`:
    - every waiter gets `errImageBuildFailed` with the log tail;
    - `image.build.result` has `outcome: failed`;
    - the index has no entry, and a later call builds again.
  - `TestResolveRebuildsWhenTheManifestIsGone` (Review Focus 3): index an entry whose digest the registry lacks, and check that it builds.
  - `TestResolveRebuildsWhenTheIndexEntryFailsVerification` (Review Focus 2): the entry's `buildSeq` points at an unrelated entry, so it rebuilds, and the bad entry is dropped.
  - `TestBuildsAreCappedByMaxConcurrentBuilds`: with `maxConcurrentBuilds: 1` and two different specs, the second build starts only after the first finishes.
  - `TestResolveRefuses`, with subtests, each `FailedPrecondition`:
    - an unpinned base (Helm fallback tag);
    - `registry.mode: external`;
    - a hostname in `buildEgress`;
    - a `buildEgress` entry covering an egress canary.
  - `TestResolveIsUnavailableWhenTheRegistryIs`: with an indexed entry and a registry server that is closed, `resolveImage` returns `Unavailable` and doesn't build.
  - `TestBuildEgressIsIntersectedWithPolicy`: `buildEgress: ["0.0.0.0/0:443", "10.0.0.0/8:80"]` with a policy allowing only `0.0.0.0/0:443`. The `BuildSpec.Egress` the fake receives holds the registry rules and `0.0.0.0/0:443` only.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/gateway/ -run 'Resolve|ConcurrentResolves|FailedBuild|BuildsAreCapped|BuildEgress'`. Expected: FAIL.

- [ ] **Step 3: Implement**

  - **Single flight:** a `map[specHash]*flight` under a mutex, where each flight carries a `done` channel, the reference and the error.
  - **The cap:** a semaphore sized from `maxConcurrentBuilds`, re-read from OperatorConfig at `prepareLeading`.
  - **The Head cache:** a map from digest to expiry, 10 minutes.
  - **Build IDs:** 16 random hex characters.
  - **Credentials:** read the pusher's at build time and the admin's only in M4c (not now).
  - **The `image.build.result` draft** comes from `audit.Writer.Do` with the start entry, as the executor does for commands. The log goes into the payload truncated as the builder returns it.
  - **Spec §11.2 edits** as listed under Files.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/gateway/ && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/gateway docs/superpowers/specs/2026-09-26-last-operator-design.md
  git commit -m "feat(gateway): resolve image specs to audited digests, building each once"
  ```

---

### Task 6: Commands and probes run on resolved images

**Files:**
- Modify: `internal/gateway/commands.go` (`runCommand`), `internal/gateway/probes.go` (`runProbes`)
- Modify: `internal/sandbox/pod.go` (`RunnerSpec` stops refusing `Image`), `internal/sandbox/probes.go` (grouping and per-group image)
- Modify: `internal/policy/policy.go` (`CheckCommand` stops refusing `Image`)
- Tests: `internal/gateway/commands_test.go`, `internal/gateway/probes_test.go`, `internal/sandbox/probes_test.go`, `internal/sandbox/pod_test.go`, `internal/policy/policy_test.go`

**Interfaces:**
- Consumes: `resolveImage`, `errImageBuildFailed`, `imageRequest` (Task 5).
- Produces:

  ```go
  // sandbox.ImageKey identifies a command's image spec for grouping: "" without install,
  // else the canonical JSON of its install list.
  func ImageKey(c lastv1alpha1.Command) string
  // ProbeRunRequest gains: Images map[string]string // ImageKey → reference; "" or missing → cfg.Pod.Image
  ```

  Probes are grouped by providers, egress and `ImageKey`, and each group's pod and `probe.start` use its image.

- [ ] **Step 1: Write the failing tests**

  - `TestRunCommandRunsOnTheResolvedImage`: a step with install, and a fake builder. `command.start`'s `image` is `localhost:5000/last/images@<digest>`, and so is the pod's container image.
  - `TestRunCommandReportsAFailedBuild`:
    - the result is `ExitCode -1`, `Error` starts with `ImageBuildFailed: `, and `Stderr` is the log tail;
    - the phase is marked failed (the next step is refused);
    - no `command.start` is written.
  - `TestRunCommandStopsOnAnUnpinnedBase`: with install and a Helm-fallback base, the call returns `FailedPrecondition` and the phase is failed.
  - `TestRunProbesGroupsByImage`:
    - two probes with different install lists and one without run in 3 pods;
    - each `probe.start` names its group's image;
    - a probe group whose build fails gets outcomes `Passed: false` with `Error` starting `ImageBuildFailed: `, and the other groups still run.
  - `TestCheckCommandAllowsImageInstall` (policy) and `TestRunnerSpecAcceptsAnImage` (sandbox). Replace the old tests that asserted the refusal.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/gateway/ ./internal/sandbox/ ./internal/policy/`. Expected: the new tests FAIL.

- [ ] **Step 3: Implement**

  - **`runCommand`:**
    - after the approval check, call `resolveImage`;
    - on `errImageBuildFailed`, set `p.failed` and return the failed result with no executor run;
    - on a `FailedPrecondition` status error, set `p.failed` and return it;
    - otherwise run on `g.executor(w, ref)`.
  - **`runProbes`:** resolve each distinct `ImageKey` first. Failed keys become failed outcomes for their probes; pass `Images` for the rest. Keep the outcome order equal to the probe order.
  - Remove `ErrImageBuildsUnsupported` and its uses.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/... && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/gateway internal/sandbox internal/policy
  git commit -m "feat(gateway): run commands and probes on their built images"
  ```

---

### Task 7: The agent, the CLI and the operator's deadlines

**Files:**
- Modify: `internal/agent/turn.go` (`wireCommand`, `buildSchema`, `command()`, `commandToWire`), `internal/agent/parse.go` (`checkCommand`), `internal/agent/prompt.go`
- Modify: `internal/cli/render.go` (`renderCommand`)
- Modify: `internal/controller/intent_execution.go` (`stepTimeout`), `internal/controller/managedresource_controller.go` (`probeTimeout`)
- Tests: `internal/agent/turn_test.go`, `internal/agent/parse_test.go`, `internal/agent/prompt_test.go`, `internal/cli/render_test.go`

**Interfaces:**
- Produces the wire field `install`, an array of string arrays. It is required and closed, and an empty array means no image. Design D9, with no union added.
  - `ParseTurn` maps a non-empty `install` to `Image: &ImageSpec{Install: [{Argv: a}…]}` and refuses an empty inner argv.
  - `commandToWire` is its inverse.

- [ ] **Step 1: Write the failing tests**

  - `TestSchemaCommandHasInstall`: the `command` definition in `$defs` has `install` as an array of arrays of strings. The existing strictness and `TestSchemaDefinesEachSharedObjectOnce` tests still pass.
  - `TestParseTurnMapsInstallToAnImage`: an empty `install` gives a nil `Image`, and `[["apt-get","install","-y","awscli=2.9.19-1"]]` maps across. `[[]]` is refused with an error naming the step.
  - `TestCommandToWireRoundTripsAnImage`: a command with an image survives `commandToWire` and back.
  - `TestPromptExplainsImageInstall`. The system prompt mentions:
    - `install`;
    - "Debian bookworm";
    - "pin";
    - that install commands run "at build time" with "no credentials".
  - `TestRenderShowsEachInstallCommand`:
    - two install argvs render as two `install:   apt-get install -y awscli=2.9.19-1` lines at the command's indent;
    - an argv element with a control character is escaped as `run:` escapes it;
    - the old "N install commands" line is gone.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/agent/ ./internal/cli/`. Expected: FAIL.

- [ ] **Step 3: Implement**

  - **The schema:** add `"install": array(strs)` to the shared `command` object.
  - **The prompt:** add one short paragraph after the sandbox description. It says the base image is Debian bookworm with `sh`, `curl`, `kubectl`, `jq`, `git` and `python3`, and no `pip`. Tools beyond it go in `install`: each argv runs as root at build time with no credentials and only the build egress, so pin versions; the image is reused while the spec is unchanged.
  - **The deadlines:** `stepTimeout = 60 * time.Minute`, `probeTimeout = 45 * time.Minute`, each with a comment saying it covers the longest build (`images.MaxBuildTimeout`) plus the command.
  - **The CLI:** render each install argv with the same quoting `run:` uses for argv.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/... && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/agent internal/cli internal/controller
  git commit -m "feat(agent): let plans bake tools into images, and show the install commands to approvers"
  ```

- [ ] **Step 6: Note for the user**

  The schema changed, so `ANTHROPIC_API_KEY=… go run ./hack/schemaprobe` should be rerun before the next real-model demo. Record that in the report; it needs the user's key.

---

### Task 8: Chart, kind install and the done-criterion e2e

**Files:**
- Modify: `charts/last-operator/values.yaml`. Add `registry.bundled.buildkitImage: moby/buildkit:v0.26.2-rootless`.
- Modify: `charts/last-operator/templates/gateway.yaml`. Gateway env `LAST_REGISTRY_FRONT_HOST`, `LAST_REGISTRY_PULL_HOST` (from `hostPort`), `LAST_BUILD_NAMESPACE` and `LAST_BUILDKIT_IMAGE`.
- Modify: `hack/kind-install.sh`. Preload the BuildKit image with `load_image`.
- Create: `test/e2e/images_e2e_test.go`
- Modify: `Makefile` (`helm-lint`: a grep that the gateway gets `LAST_BUILDKIT_IMAGE`)
- Modify: `hack/demo.sh` and `README.md` (the opt-in awscli demo, design §9)

**Interfaces:**
- Consumes everything above; the e2e helpers in `test/e2e`: `e2eClients`, `waitFor`, `kubectl`, `runLast` / `runLastInput`, `verifyAuditChain`, and the S3 audit read in `intentEntryTypes` (generalise it into a helper that returns whole entries for an Intent, or for all entries).

- [ ] **Step 1: Write the e2e** (`TestImageBuildIsReused`, `//go:build e2e`)

  Model it on `TestOperatorWebServer`: a unique suffix, a namespace, a `kubernetes` Provider, a scripted LLM ConfigMap, OperatorConfig changes restored in cleanup, and approval through `kubectl last approve --hash`. The scripted plan has:
  - a step `s1` with `install: [["python3","-c","import os; p='/usr/local/bin/last-e2e-tool'; open(p,'w').write('#!/bin/sh\\necho last-e2e-tool-ok\\n'); os.chmod(p,0o755)"]]` and `run.argv: ["last-e2e-tool"]`;
  - one resource whose probe runs the same command with the same install and asserts `result.exitCode == 0 && result.stdout.contains('last-e2e-tool-ok')`.

  Assertions:
  - The first Intent reaches Ready.
  - A second Intent, in the same namespace with the same scripted plan, reaches Ready.
  - Across the whole chain, exactly one `image.build.start` has this spec hash. Read its `specHash` from the first `image.build.start` of the first Intent.
  - Every `command.start` and `probe.start` of both Intents names the same `localhost:5000/last/images@sha256:…` image.
  - `verifyAuditChain` passes.

  Add a second test, `TestImageBuildFailureFailsTheStep`, with install `[["false"]]`. The Intent ends `Failed`, and its Executed condition message (or the status the CLI prints) contains `ImageBuildFailed`.

- [ ] **Step 2: Change the chart, kind install and demo**

  - **The chart and kind install:** make the edits under Files.
  - **`hack/demo.sh`:**
    - takes `DEMO_REQUEST` (default unchanged);
    - sets `OperatorConfig.spec.images.buildEgress: ["0.0.0.0/0:80", "0.0.0.0/0:443"]`;
    - adds those two entries to `spec.defaults.egress`, so the policy intersection keeps them.
  - **The README** documents `DEMO_REQUEST="install the AWS CLI in an image and print its version" make demo` as the M4 real-model check. It builds from Debian mirrors and spends API credit.
  - Then run `make helm-lint` and `make nginx-check`. Expected: PASS.

- [ ] **Step 3: Run the full e2e on the existing cluster**

  Run `KIND_CLUSTER=last-m4a hack/e2e.sh > "$SCRATCH/m4b-e2e.log" 2>&1`, where `$SCRATCH` is the session's scratchpad. Expected: `ok github.com/moogie-labs/last-operator/test/e2e`, with both new tests passing and every earlier test still passing.

- [ ] **Step 4: Commit**

  ```bash
  git add charts hack/kind-install.sh hack/demo.sh README.md test/e2e Makefile
  git commit -m "test(e2e): build an image once and reuse its digest across commands, probes and Intents"
  ```
