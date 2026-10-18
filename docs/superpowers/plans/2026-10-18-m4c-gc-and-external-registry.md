# M4c: Image Garbage Collection and External Registry Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reclaim the bundled registry's storage from images nothing uses, and let installs keep built images in an external registry (ECR, GHCR, Harbor) instead.

**Architecture:**
- **Garbage collection** runs on the leading gateway. It picks index entries unused for `images.unusedRetention` that no Intent or ManagedResource references, pauses builds, deletes their manifests with the admin credential, and drops them from the index. It then asks a new sidecar in the registry pod, `registry-gc`, to run `registry garbage-collect`, and audits every step.
- **External mode** generalises the build, run and delete paths from the bundled hosts to `registry.external.repository`. It pushes with `pushSecretRef`'s Docker config, and copies `pullSecretRef` into `last-sandbox` for kubelet.

**Tech Stack:** Go 1.26, client-go, go-containerregistry v0.20.3, `golang.org/x/crypto/bcrypt`, Distribution `registry:3.0.0`, Helm, kind.

**Spec:** `docs/superpowers/specs/2026-10-17-m4-image-builds-design.md`:
- §6 (6.1 garbage collection, 6.2 external mode), §7, §9 (the M4c parts), and §10 amendments;
- the main spec, `docs/superpowers/specs/2026-09-26-last-operator-design.md` §11.2–§11.3.

Earlier work is in `docs/superpowers/plans/2026-10-17-m4b-image-builds.md` and the M4a and M4b follow-ups in `docs/superpowers/followups/`.

**Verified during planning (Distribution `registry:3.0.0`):**
- A `DELETE /v2/<repo>/manifests/<digest>` returns 202 and also removes the tag.
- `registry garbage-collect /etc/distribution/config.yml`, with no flags, then frees the blobs: the registry shrank from 2008 KB to 88 KB.
- On a registry with no repositories yet, `garbage-collect` fails with "Path not found: …/repositories".

## Global Constraints

- **Commits:** never add a commit attribution trailer (such as `Co-authored-by:`). Never commit `.superpowers/` or logs.
- **Credentials:**
  - the admin password, the push Docker config and its `auth` strings never appear in logs, errors, audit payloads or command lines;
  - the sidecar receives only bcrypt htpasswd lines.
- **Sidecar:**
  - binary `cmd/registry-gc`, image `images/registry-gc/Dockerfile` (`FROM registry:3.0.0` plus the binary), published as `last-registry-gc`;
  - listens on `:5001` and serves only `POST /gc`;
  - requires basic auth matching the `admin` line of `/etc/registry-gc/htpasswd` (the `last-registry-auth` Secret's `admin` key);
  - runs `registry garbage-collect /etc/distribution/config.yml`;
  - answers `200 {"bytesFreed": <int64>, "log": "<last 64 KiB>"}`;
  - answers 409 while a run is in progress, 401 for bad credentials and 405 for other methods;
  - treats an absent `/var/lib/registry/docker/registry/v2/repositories` as `{"bytesFreed": 0}` without running anything.
- **Chart:**
  - the sidecar container `registry-gc` sits in the `last-registry` pod and shares its PVC at `/var/lib/registry`;
  - the Service `last-registry-gc` serves port 5001;
  - the NetworkPolicy `last-registry` also admits port 5001 from gateway pods only;
  - the value `registry.bundled.gcImage` defaults to `{{ include "last.image" (list . "registry-gc") }}`.
- **Gateway configuration:**
  - `LAST_IMAGE_GC_INTERVAL` (default `24h`). The first run comes one interval after leadership starts.
  - `LAST_REGISTRY_GC_URL` (default `http://last-registry-gc.<registry ns>.svc:5001`).
  - `images.unusedRetention` (default `720h`, 30 days).
- **Audit payloads:**

  | Entry | Fields |
  |---|---|
  | `image.gc.start` | `{"retention", "candidates": [specHash...]}` |
  | `image.deleted` | `{"specHash", "digest", "registryDeleted": bool, "error"}` |
  | `image.gc.result` | `{"deleted": int, "bytesFreed": int64, "sidecarError", "durationMs"}` |

  All are written by the gateway actor with no Intent.
- **GC order** (design §6.1):
  1. write `start`;
  2. pause new builds and wait for running ones;
  3. for each candidate, drop it from the index, delete its manifest, and write `image.deleted`;
  4. call the sidecar (bundled mode only);
  5. resume builds;
  6. write `result`.

  Builds resume on every path, including errors and cancellation.

  Dropping the index entry before deleting the manifest is deliberate; design §6.1 lists the deletion first. The order means no resolve can hand out a digest that is about to disappear (Review Focus 3).
- **Candidates:** index entries whose `lastUsed` is older than the retention, minus the referenced spec hashes. A spec hash is referenced when it is computed with the current pinned base from any install in an Intent's `status.plan` (steps, probes, teardown) or a ManagedResource's probes. References are only a hint (design §6.1).
- **External mode** (design §6.2):

  | Use | Reference |
  |---|---|
  | push | `<repository>:sha256-<hex>` |
  | run | `<repository>@<digest>` |

  - The push config is `pushSecretRef`'s `.dockerconfigjson`, passed to the build as `/work/.docker/config.json`, with no `http = true`.
  - The base is used unchanged. HEAD and DELETE use the push config's credentials over HTTPS.
  - The pull secret is copied to `last-sandbox/last-registry-pull`, and sandbox pods list it in `imagePullSecrets`.
  - A DELETE answered 405, or with the error code `UNSUPPORTED`, gives `registryDeleted: false`, and the entry still leaves the index.
  - There is no sidecar call.
- **After Go changes:** run `make lint-fix`, `make lint` (0 issues) and `make test`. **After chart changes:** run `make helm-lint` and `make nginx-check`.

## Review Focus

1. **GC must not hold up builds for long, or leave them stuck.** A GC that fails midway, whether from a manifest DELETE error, the sidecar being down or cancellation, must still resume builds. Task 3 tests each path with a build waiting.
2. **An image some Intent still references must survive GC** even when its `lastUsed` is old. Task 3 tests the reference hint, and Task 6 tests it on kind.
3. **A resolve racing with GC must never get a deleted digest.** GC drops the index entry before deleting the manifest, and a resolve during the pause waits. Task 3 tests that a resolve blocked on the pause rebuilds after GC deletes its image.
4. **The sidecar must reject everyone but the admin, and must never run twice at once.** Task 1 tests a missing or wrong password, the pusher's password, a concurrent POST (409) and GET (405).
5. **External registries that refuse DELETE (ECR, GHCR) must not wedge GC.** The entry leaves the index and the result records `registryDeleted: false`. Task 5 tests a 405 and an `UNSUPPORTED` body.

---

### Task 1: The `registry-gc` sidecar

**Files:**
- Create: `internal/registrygc/server.go`, `internal/registrygc/server_test.go`
- Create: `cmd/registry-gc/main.go`
- Create: `images/registry-gc/Dockerfile`
- Modify: `Makefile`. Add an `image-registry-gc` target built like `image-sandbox-base`, and make `images` depend on it.
- Modify: `hack/kind-install.sh`. Load `last-registry-gc:$TAG` alongside the other images.
- Modify: `go.mod`. Promote `golang.org/x/crypto` to a direct dependency.

**Interfaces:**
- Produces:

  ```go
  package registrygc
  type Config struct {
      HtpasswdFile string        // reads the "admin:" line
      ConfigFile   string        // /etc/distribution/config.yml
      DataDir      string        // /var/lib/registry
      Run          func(ctx context.Context, name string, args ...string) ([]byte, error) // default: exec.CommandContext(...).CombinedOutput
  }
  func Handler(cfg Config) http.Handler // POST /gc only
  ```

  `main` serves `Handler` on `:5001`. Its flags have the defaults listed in Global Constraints.

- [ ] **Step 1: Write the failing tests** (`httptest`, a temporary `DataDir` holding files, a fake `Run`)

  - `TestGCRunsGarbageCollectAndReportsBytesFreed`. The fake `Run` deletes a 10 000-byte file under `DataDir/docker/registry/v2/repositories/…`. The response is 200 with `bytesFreed` ≥ 10000, and `Run` was called with `registry garbage-collect <ConfigFile>`.
  - `TestGCOnAnEmptyRegistryFreesNothing`: with no `repositories` directory, the response is 200 with `bytesFreed: 0`, and `Run` is never called.
  - `TestGCRejects`, with subtests:
    - no auth: 401;
    - the admin user with a wrong password: 401;
    - the pusher's credential: 401;
    - `GET /gc`: 405;
    - `POST /other`: 404.
  - `TestGCRefusesAConcurrentRun`: while a `Run` is blocked, a second POST gets 409.
  - `TestGCReportsAFailedRun`: `Run` returns an error with output. The response is 500, and its JSON `log` holds the output tail, never the password.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/registrygc/`. Expected: FAIL to compile.

- [ ] **Step 3: Implement**

  - **Auth:** `bcrypt.CompareHashAndPassword` against the hash after `admin:`.
  - **bytesFreed:** a `filepath.WalkDir` size sum before and after the run.
  - **Single run:** a `sync.Mutex` held with `TryLock`.
  - **Log:** the last 64 KiB of the output.
  - **The Dockerfile** builds the binary as the root Dockerfile does (CGO off), then `FROM registry:3.0.0`, `COPY` it to `/usr/local/bin/registry-gc`, `USER 65532`, `ENTRYPOINT ["/usr/local/bin/registry-gc"]`.

- [ ] **Step 4: Run the tests and build the image**

  Run `go test ./internal/registrygc/ && make lint && make image-registry-gc IMAGE_TAG=dev`. Expected: PASS, and the image builds.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/registrygc cmd/registry-gc images/registry-gc Makefile hack/kind-install.sh go.mod go.sum
  git commit -m "feat(registry-gc): add the sidecar that runs Distribution's garbage collection on request"
  ```

---

### Task 2: Chart — the sidecar, its Service and its NetworkPolicy

**Files:**
- Modify: `charts/last-operator/values.yaml` (`registry.bundled.gcImage`, `registry.bundled.gcResources`)
- Modify: `charts/last-operator/templates/registry.yaml`
- Modify: `charts/last-operator/templates/gateway.yaml` (env `LAST_IMAGE_GC_INTERVAL` from the new `gateway.imageGCInterval`, default `24h`, and `LAST_REGISTRY_GC_URL`)
- Modify: `Makefile` (`helm-lint` greps)

**Interfaces:**
- Consumes: the image from Task 1.
- Produces:
  - the `registry-gc` container in the `last-registry` Deployment;
  - the Service `last-registry-gc:5001`;
  - gateway env vars for Tasks 3 and 6.

- [ ] **Step 1: Add the failing `helm-lint` checks**

  - The `last-registry` Deployment has a container `registry-gc`, with the PVC mounted at `/var/lib/registry`, and `last-registry-auth` mounted with only key `admin`, at `/etc/registry-gc/htpasswd`.
  - The render contains `name: last-registry-gc` with `port: 5001`.
  - The `last-registry` NetworkPolicy has an ingress rule for port 5001 from `app.kubernetes.io/name: last-gateway` in `namespaces.gateway`, and no other peer on 5001.
  - The gateway has `LAST_IMAGE_GC_INTERVAL` and `LAST_REGISTRY_GC_URL`.

- [ ] **Step 2: Run the checks and confirm they fail**

  Run `make helm-lint`. Expected: FAIL.

- [ ] **Step 3: Write the templates**

  The sidecar's security context matches the registry container (UID 65532, drop ALL, RuntimeDefault seccomp). Mount the same Distribution config the registry container uses, and give it resources from values.

- [ ] **Step 4: Run the checks**

  Run `make helm-lint && make nginx-check`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add charts Makefile
  git commit -m "feat(chart): run the registry GC sidecar, reachable only from the gateway"
  ```

---

### Task 3: Gateway garbage collection

**Files:**
- Create: `internal/gateway/imagegc.go`, `internal/gateway/imagegc_test.go`
- Modify: `internal/gateway/images.go`. Add `imageBuilds.pause(ctx) (resume func(), err error)`: it blocks new `acquire`s and waits until running builds release their slots.
- Modify: `internal/gateway/config.go` (`ImageGCInterval`, `RegistryGCURL`), `internal/gateway/server.go` (schedule the run)
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §11.2 "Garbage collection" and §11.3 "Coordinated GC". Make them match the order and payloads in Global Constraints.

**Interfaces:**
- Consumes: `g.images` (`entries`, `drop`), `images.SpecHash`, `images.BaseDigest`, `g.sandboxImage`, `images.Client.Delete`, the `last-registry-admin` Secret (`username`, `password`), `audit.TypeImageGCStart`, `TypeImageDeleted`, `TypeImageGCResult`.
- Produces:

  ```go
  // imageIndex gains: snapshot returns a copy of every entry, under the lock.
  func (x *imageIndex) snapshot() map[string]imageEntry
  type gcResult struct{ Deleted int; BytesFreed int64; SidecarError string }
  // runImageGC performs one collection (Global Constraints, GC order).
  func (g *Gateway) runImageGC(ctx context.Context, w *audit.Writer) (gcResult, error)
  // referencedSpecs returns the spec hashes the cluster's Intents and ManagedResources still use.
  func (g *Gateway) referencedSpecs(ctx context.Context, baseDigest string) (map[string]bool, error)
  // imageGCDeleter abstracts the registry delete so Task 5 can add external mode:
  type imageGCDeleter interface{ Delete(ctx context.Context, repo, digest string) error }
  ```

  The leader loop starts one GC goroutine per `ImageGCInterval` tick, and never two at once. The goroutine is tracked in `g.inflight`, uses the leader context, and logs its outcome.

- [ ] **Step 1: Write the failing tests**

  Use the existing image test env: the fake builder, the in-memory registry with the admin middleware, and the audit test sink.
  - `TestImageGCDeletesUnusedImages`:
    - two indexed images, one with `lastUsed` 31 days ago and no reference, one used an hour ago;
    - only the old one is deleted from the registry and the index;
    - `image.gc.start` lists it, and `image.deleted` names it with `registryDeleted: true`;
    - `image.gc.result` has `deleted: 1` and the sidecar's `bytesFreed` (a fake sidecar `httptest` server returns 1234).
  - `TestImageGCKeepsReferencedImages` (Review Focus 2): an old image whose install appears in an Intent's `status.plan` probe survives, and so does one in a ManagedResource probe.
  - `TestImageGCPausesAndResumesBuilds` (Review Focus 1, 3):
    - a build running when GC starts finishes first;
    - a resolve started during GC waits, then rebuilds its deleted image;
    - builds resume after GC fails in each way: a DELETE 500, the sidecar returning 500, and context cancellation.
  - `TestImageGCRecordsASidecarFailure`: sidecar 500, so the result has a `sidecarError` and builds resume.
  - `TestImageGCRunsOnlyOneAtATime`: two ticks while one run is blocked give one run.
  - `TestImageGCNeverAuditsTheAdminPassword`: scan every entry.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/gateway/ -run ImageGC`. Expected: FAIL.

- [ ] **Step 3: Implement**

  - **The sidecar call:** `POST` to `RegistryGCURL + "/gc"` with the admin basic auth and a 30-minute timeout.
  - **Pause:** take every slot of the current semaphore while a `paused` flag makes new `acquire`s wait on a resume channel.
  - **Spec edits** as listed under Files.

- [ ] **Step 4: Run the tests and lint**

  Run `go test -race ./internal/gateway/ && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/gateway docs/superpowers/specs/2026-09-26-last-operator-design.md
  git commit -m "feat(gateway): collect unused images daily, pausing builds and auditing every deletion"
  ```

---

### Task 4: External mode in `internal/images`

**Files:**
- Modify: `internal/images/buildpod.go`, `internal/images/buildpod_test.go`
- Create: `internal/images/dockerconfig.go`, `internal/images/dockerconfig_test.go`

**Interfaces:**
- Produces:

  ```go
  // CredentialsFor returns the basic credentials a Docker config JSON holds for host
  // ("auths"[host].auth, base64 "user:pass"), or nil when it has none.
  func CredentialsFor(dockerConfig []byte, host string) (*Credentials, error)
  // DockerConfig builds a config.json for one host and credential (bundled mode).
  func DockerConfig(host, user, password string) []byte
  // BuildSpec: PushUser/PushPassword are replaced by PushConfig []byte (a config.json).
  // BuildPodConfig gains PushRef string: the push target "<host>/<repo>:<tag>" prefix
  //   without the tag ("<FrontHost>/last/images" bundled, the repository external),
  //   and Insecure bool (true only for the bundled front host, which also writes
  //   http = true into buildkitd.toml; external writes no registry section).
  ```

  Redaction of a build's log covers every `auth` value in `PushConfig`, its decoded `user:password`, and the password.

- [ ] **Step 1: Write the failing tests**

  - `TestCredentialsFor`: it finds the host, returns nil for a missing host, and errors on malformed JSON or base64.
  - `TestBuildObjectsPushToTheExternalRepository`: with `PushRef: "123.dkr.ecr.us-east-1.amazonaws.com/last-images"` and `Insecure: false`, the script pushes `…/last-images:sha256-<hex>` with no `registry.insecure=true`, and `buildkitd.toml` has no registry section. The `FROM` base is unchanged.
  - `TestBuildObjectsKeepTheBundledDefaults`: the existing bundled tests pass after switching to `PushConfig: DockerConfig(front, "pusher", pw)`.
  - `TestBuilderRedactsEveryPushConfigCredential`: an external config with two hosts. Neither `auth` value, nor either decoded password, survives in `Log`.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/images/`. Expected: FAIL.

- [ ] **Step 3: Implement**

  Update the gateway's call sites in `internal/gateway/images.go`, so they still compile with bundled behaviour unchanged: `PushConfig` from `DockerConfig(frontHost, user, password)`, `PushRef` and `Insecure` set for bundled.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/images/ ./internal/gateway/ && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/images internal/gateway/images.go
  git commit -m "feat(images): push builds to any registry from a Docker config"
  ```

---

### Task 5: External mode in the gateway and sandbox

**Files:**
- Modify: `internal/gateway/images.go`:
  - drop the external-mode FailedPrecondition;
  - choose bundled or external references, clients and push config;
  - add no front-proxy egress rule in external mode.
- Modify: `internal/gateway/imagegc.go`. External deletes use the push credentials; no sidecar call.
- Create: `internal/gateway/pullsecret.go`. Copy `pullSecretRef` to `last-sandbox/last-registry-pull` (create, or delete and create when it differs), before any sandbox run in external mode.
- Modify: `internal/sandbox/pod.go` (`PodConfig.ImagePullSecrets []string`), `internal/gateway/runtime.go` (set it in external mode)
- Modify: `charts/last-operator/templates/gateway.yaml`. In `last-gateway-sandbox`, add `get` to `secrets` so the copy can compare.
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §11.3 "External". Record the 405/UNSUPPORTED behaviour and the pull-secret copy.

**Interfaces:**
- Consumes: Task 4's `CredentialsFor`, `PushConfig`, `PushRef`, `Insecure`; Task 3's `imageGCDeleter`.
- Produces: external mode end to end, behind `OperatorConfig.spec.registry.mode: external`.

- [ ] **Step 1: Write the failing tests**

  These use a TLS `httptest` in-memory registry as the "external" registry and a push Secret whose config names its host.
  - `TestResolveBuildsIntoTheExternalRepository`: the fake builder receives `PushRef = <repository>`, `Insecure = false`, and the Secret's config. The result is `<repository>@<digest>`. A second resolve HEADs with the push credentials and doesn't build.
  - `TestExternalModeCopiesThePullSecret`: a run in external mode creates `last-sandbox/last-registry-pull` from `pullSecretRef`, the pod lists it in `imagePullSecrets`, and a changed source Secret is re-copied.
  - `TestImageGCInExternalMode` (Review Focus 5): a DELETE answered 405, and one answered 400 with `{"errors":[{"code":"UNSUPPORTED"}]}`, both give `image.deleted{registryDeleted:false}`. The entry leaves the index and no sidecar is called.
  - `TestExternalModeRefusesAMissingPushSecret`: FailedPrecondition, audited through `d.deny`.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/gateway/ ./internal/sandbox/`. Expected: FAIL.

- [ ] **Step 3: Implement**

  - The push and pull Secrets are type `kubernetes.io/dockerconfigjson`, key `.dockerconfigjson`, in the gateway namespace.
  - Parse the registry host as the first path segment of `repository`.
  - Make the spec edits listed under Files.

- [ ] **Step 4: Run the tests and lint**

  Run `go test -race ./internal/... && make lint && make helm-lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal charts docs/superpowers/specs/2026-09-26-last-operator-design.md
  git commit -m "feat(gateway): build into and run from an external registry"
  ```

---

### Task 6: The GC e2e

**Files:**
- Modify: `hack/kind-install.sh`. Pass `--set gateway.imageGCInterval=1m`.
- Create: `test/e2e/imagegc_e2e_test.go`
- Modify: `README.md` (one paragraph on image retention and GC)

**Interfaces:**
- Consumes: everything above, and the M4b e2e helpers in `test/e2e/images_e2e_test.go` (the Intent setup, `auditEntries`).

- [ ] **Step 1: Write `TestImageGCReclaimsUnusedImages`**

  Model it on `TestImageBuildIsReused`.
  1. **Build two images.** Two Intents with different unique installs reach Ready, giving images A and B.
  2. **Release A.** Delete A's Intent and wait until its ManagedResources are gone.
  3. **Shrink the retention.** Set `OperatorConfig.spec.images.unusedRetention: 1s` (restored in cleanup).
  4. **Wait up to 4 minutes for collection.** An `image.gc.result` with `deleted >= 1` and `bytesFreed > 0` appears, written after the retention change.

  Assertions:
  - an `image.deleted` names A's spec hash;
  - no `image.deleted` names B's spec hash;
  - B's Intent stays Ready, and its probes still pass after the GC;
  - a new Intent with A's install starts a new `image.build.start` for A's spec hash, and reaches Ready.

- [ ] **Step 2: Run the full e2e on the existing cluster**

  Run `KIND_CLUSTER=last-m4a hack/e2e.sh > "$SCRATCH/m4c-e2e.log" 2>&1`, where `$SCRATCH` is the session's scratchpad. Expected: `ok … test/e2e`, with every test passing.

- [ ] **Step 3: Commit**

  ```bash
  git add hack/kind-install.sh test/e2e README.md
  git commit -m "test(e2e): garbage collection frees an unused image and keeps a referenced one"
  ```
