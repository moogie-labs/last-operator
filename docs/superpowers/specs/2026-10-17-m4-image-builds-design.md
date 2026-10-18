# M4: Image Builds and Registry — Design

**Status:** drafted on 2026-10-17 from the main spec and the M4 spike. The user chose to skip the brainstorming questions. Calls made without them are marked *(my call)*.

**Parent spec:** `docs/superpowers/specs/2026-09-26-last-operator-design.md` (the "main spec"). This document designs milestone M4 (main spec §20). The amendments in §10 carry its decisions back into the main spec. Where the two disagree, the amended main spec is authoritative.

**Inputs:**
- the spike, `docs/superpowers/spikes/2026-10-17-m4-registry-spike.md`;
- the M3c follow-ups ("M4 image builds": render the install commands).

## 1. Goal and done-criterion

M4 lets a command use tools that are not in the base image. The gateway bakes the command's `image.install` commands into an image, pins it by digest, records it in the audit log, and runs the command, and every later command and probe with the same image spec, on that digest.

**Done when:** on kind, an Intent whose plan has a command needing a tool outside the base image builds that image once. The command, the Intent's probes, and a second Intent with the same image spec then all run on the recorded digest with no rebuild. `kubectl last audit verify` passes (main spec §20).

**Also delivered:**
- the bundled registry (Distribution v3, the nginx front proxy, the node-local pull proxy);
- external registry mode;
- coordinated garbage collection;
- a portable check of the pull proxy that can be run on any cluster, including EKS and GKE Standard.

## 2. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **The pull-proxy pattern stands as the bundled default.** The spike validated it on kind and k3s with no containerd configuration. EKS and GKE Standard are checked with `hack/registry-check.sh` (§3.5) when someone has those clusters. A failure there makes that platform external-registry only; it does not change the design. | The spike showed containerd 2.x falls back to plain HTTP on `localhost`, and the hostPort works on portmap-based CNIs. |
| D2 | **The digest index is persisted and checked against the audit log, not rebuilt by scanning it.** The gateway keeps `specHash → {digest, buildSeq, lastUsed}` in a ConfigMap. On first use in a process, it reads the signed `image.build.result` entry at `buildSeq` and checks the seal, the type, the spec hash and the digest (the `readIndexed` pattern). A lost index means images get rebuilt; it never means an unaudited digest runs. | Main spec §11.2 says to rebuild at startup from `image.build.result` entries. At the audit volumes main spec §17.2 projects (100M+ entries a year), a full scan at every start is not viable. The audit log stays authoritative, because only a digest that a verified entry names can run. |
| D3 | **Build pods run in a new namespace, `last-build`.** It is labelled `privileged` for Pod Security, has a default-deny NetworkPolicy, and the gateway is the only writer. | Rootless BuildKit needs seccomp and AppArmor `Unconfined` (spike finding 6), which the baseline level forbids. `last-sandbox` keeps enforcing restricted. |
| D4 | **Rootless BuildKit runs without the process sandbox** (`--oci-worker-no-process-sandbox`). | It is the only configuration that worked on kind (spike finding 6). Main spec §17.2 already accepts that builds are less isolated. Compensating controls: no credentials but the pusher's, limited egress, resource and time limits. |
| D5 | **Build egress accepts only IPs and CIDRs, and adds DNS.** `images.buildEgress` uses the sandbox egress parser, which refuses hostnames (`ErrHostEgressUnsupported`). Its default becomes empty, meaning a build reaches only the registry and DNS. Operators who want public package sources set CIDRs, such as `0.0.0.0/0:443`. Build pods may also reach kube-dns on port 53. | Hostname egress needs an FQDN-aware CNI, which the project does not support yet, so the main spec's default list (`pypi.org:443`, …) would fail every build. Package installs need DNS. A build holds no credentials, so DNS exfiltration risks nothing secret. |
| D6 | **Builds need a digest-pinned base.** A command with `image.install` fails with `FailedPrecondition` while the base image is only the Helm fallback tag. The base may live in the bundled registry as `localhost:5000/last/base@sha256:…`. The build then pulls it through the front proxy, and sandbox pods pull it through the pull proxy. | The spec hash includes `baseDigest`. A build pod cannot read the nodes' containerd store (spike finding 7). |
| D7 | **Builds run inside `RunCommand` and `RunProbes`.** They run before `command.start`/`probe.start`, only for commands in the approved open phase or the approved probe set, and as single flight per spec hash. | No new RPC: the approval and phase checks already guard these calls. An unapproved install never builds. |
| D8 | **Pull failures are transient for a grace period.** For 60 s from the first `ErrImagePull`/`ImagePullBackOff`, the executor waits for kubelet's retry instead of failing the command. `InvalidImageName` still fails at once. | A node's pull proxy restarting costs one kubelet backoff (spike finding 3). Failing fast would fail the command. |
| D9 | **Turns carry `image` as a closed object.** It is always present, with `install` set to `[]` for none, so it adds no union. | Strict mode needs every property required. A union would count against Anthropic's limits, and the grammar-size failure of M3c is fresh. |
| D10 | **M4 is three plans:** M4a, the registry; M4b, builds end to end; M4c, garbage collection and external mode. | M4a is useful and testable alone (a registry and a pull check). M4b depends on it, and M4c on M4b's index. It matches the M2 and M3 splits. |

## 3. Bundled registry (M4a)

### 3.1 Components

The chart installs these in `last-registry` when `registry.bundled.enabled` is true (the default):

- **Distribution v3** (`registry:3`, exact tag pinned) on a PVC (`registry.bundled.size`, default 20Gi), with `REGISTRY_STORAGE_DELETE_ENABLED=true`. A Service named `last-registry`.
  - A NetworkPolicy allows ingress only from the front proxy and from the GC sidecar's own pod.
  - The GC sidecar shares the pod; it arrives in M4c.
- **Front proxy:** an nginx Deployment and a Service, `last-registry-front`. It carries the spike's `map $request_method` configuration (spike finding 4): GET and HEAD anonymous, DELETE admin only, every other method pusher only.
  - Ingress is allowed from the pull-proxy pods, from `last-build`, and from the gateway.
  - Its access log is on, without credentials.
- **Pull proxy:** an nginx DaemonSet on `hostPort` 5000, tolerating all taints. It allows only the paths kubelet needs (spike finding 5):
  - `GET`/`HEAD` on `^/v2/$`;
  - `GET`/`HEAD` on `^/v2/last/(images|base)/(manifests|blobs)/sha256:[0-9a-f]{64}$`.

  Everything else gets 403, including tag reads, `_catalog` and every write method.
- **Credentials.** The pusher and admin passwords are generated at install with `last.persistedSecretValue`, kept with `helm.sh/resource-policy: keep`, and stored as htpasswd entries in the front proxy's Secret. Plain copies go to:
  - `last-gateway/last-registry-admin`, read only by the gateway;
  - `last-gateway/last-registry-pusher`, copied by the gateway into each build's Secret.

  The operator's RBAC covers neither namespace.
- **Namespace labels:** `last-registry` and `last-build` are `privileged` (hostPort; D3). `last-sandbox` is unchanged.

### 3.2 Image references

| Use | Bundled | External |
|---|---|---|
| Run (kubelet) | `localhost:5000/last/images@<digest>` | `<repository>@<digest>` with `pullSecretRef` |
| Build push target | `last-registry-front.last-registry.svc:5000/last/images:<tag>` | `<repository>:<tag>` |
| Base in the registry | `localhost:5000/last/base@<digest>` (run) / `…front…/last/base@<digest>` (build `FROM`) | as configured |

The tag is `sha256-<spec hash hex>`. It is only a label for humans: only digests from verified audit entries are ever run (main spec §11.3).

### 3.3 Registry client

`internal/images` holds a small OCI distribution client, built on `go-containerregistry` *(my call; the spike used its crane, and it handles auth and media types)*:

- `Head(digest)`: does the manifest exist;
- `Delete(digest)`: admin only;
- `Push`: used only by tests and the e2e base-image push.

### 3.4 e2e base image

The kind install script pushes the locally built sandbox base into the bundled registry as `last/base`, through a port-forward to the front proxy with the pusher credential. It then sets `OperatorConfig.sandbox.baseImage` to `localhost:5000/last/base@<digest>`. From then on, M4 e2e and demo runs use a pinned base, and sandbox pods pull it through the pull proxy.

### 3.5 `hack/registry-check.sh`

A script anyone can run against any kube context with the chart installed:
- push a small image with the pusher credential;
- run a pod from `localhost:5000/last/images@<digest>` on **every** schedulable node;
- check that each node's proxy served the pull;
- check that write methods and tag reads are refused.

It prints a per-node table. This is how EKS and GKE Standard get validated (D1), and the result goes in the spike doc.

## 4. Builds (M4b)

### 4.1 Resolving an image spec

For each command (or probe group) with a non-empty `image.install`:

1. **The spec hash:** `specHash = sha256(canonical {baseDigest, install})` (main spec §8.1). `baseDigest` comes from the pinned base (D6).
2. **Index hit.** The gateway verifies the build entry once per process (D2). It then checks the manifest with `Head`, caching a success for 10 minutes. On a hit it updates `lastUsed` (in memory; flushed with the other gateway state, at most once an hour per entry) and returns the digest.
3. **Miss, or a manifest gone.** It builds (§4.2) under single flight: concurrent requests for one spec hash wait on one build, and a failed build fails every waiter.
   - At most `images.maxConcurrentBuilds` builds run at once (default 2). The rest queue.
   - Build time does not count against the command's timeout. The operator's RPC deadline for a step or a probe run must cover `images.buildTimeout` plus the command timeout plus slack.
4. **The audit record.** `command.start` and `probe.start` record the resolved image reference and digest for every command (main spec §8.1).

### 4.2 The build

1. **Write `image.build.start`.** It records:
   - the spec hash, base digest, the generated Containerfile and the push tag;
   - the requesting Intent, stage and subject hash. The image is shared, but the record says who first needed it.
2. **Generate the Containerfile** exactly as main spec §11.2 shows: `FROM <base>@<digest>`, `USER root`, one exec-form `RUN` per install argv, then `USER 65532`. Build arguments, `COPY` and heredocs are never generated.
3. **Create the build's objects in `last-build`:**
   - a ConfigMap with the Containerfile and `buildkitd.toml`, marking the front proxy `http = true` in bundled mode;
   - a Secret with a Docker `config.json` holding the push credential;
   - a NetworkPolicy selecting only that pod. It allows the registry, with both the ClusterIP and the endpoint addresses, as `APIServerEgress` does today; kube-dns on UDP and TCP 53; and `images.buildEgress` intersected with the requesting Intent's policy egress (`policy.Egress()`);
   - the pod: rootless BuildKit with the spike's configuration (spike finding 6), `automountServiceAccountToken: false`, CPU 2, memory 4Gi, ephemeral storage 10Gi, and `activeDeadlineSeconds` from `images.buildTimeout` (default 15m).

   The egress gate (main spec §11.1) applies to build pods too. The build is wrapped, so the result line is printed only after the canaries are confirmed closed.
4. **Read the result.** The gateway reads the digest from a final result line that the wrapper prints from `--metadata-file`, then confirms it with `Head`. The build log, meaning the pod log with the result line removed, is truncated at 1 MiB and has the push credential redacted (raw and base64), as command output is.
5. **Write `image.build.result`:** spec hash, digest, outcome, duration and log. On success, add the digest to the index; on failure, keep it out.
6. **Delete the pod, Secret, ConfigMap and NetworkPolicy,** whatever the outcome. At startup and on gaining leadership, the gateway deletes leftover build objects by label, as it does for sandbox runs.

### 4.3 A failed build

The command fails, and its result carries the reason (`ImageBuildFailed`) and the build log's tail (64 KiB) as stderr.
- **During execution:** the step fails as any failed step does.
- **During discovery:** the model sees the log and can change the install commands.
- **During probes:** the probe fails with the same reason.

A build that hits its deadline fails the same way.

## 5. Operator, agent and CLI (M4b)

- **Policy.** `policy.go` stops rejecting `image.install`. `sandbox.RunnerSpec` stops returning `ErrImageBuildsUnsupported`.
- **Turn schema.** The `command` definition gains `"image": {"install": [{"argv": [string]}]}`, closed and required (D9). `ParseTurn` maps an empty `install` to a nil `Image`. Rerun `go run ./hack/schemaprobe` before M4b merges.
- **Prompt.** The system prompt describes:
  - the base image (Debian bookworm with `sh`, `curl`, `kubectl`, `jq`, `git`, `python3`; no `pip`);
  - when to use `image.install`;
  - that install commands run as root at build time, with no credentials and only the build egress;
  - that versions must be pinned (`apt-get install -y awscli=2.9.19-1`), since an image is frozen at build time.
- **Timeouts.** The operator's step and probe deadlines grow by `images.buildTimeout` (§4.1 step 3).
- **CLI.** `renderCommand` prints each install command on its own `install:` line, escaped like `run:`. That replaces "N install commands" (M3c follow-up). The install commands are inside the plan hash already, so an approver sees exactly what will be baked in.
- **API.** `ImagesConfig` gains `buildTimeout` (default 15m, max 30m) and `maxConcurrentBuilds` (default 2, 1–8). Run `make manifests generate`.

## 6. Garbage collection and external mode (M4c)

### 6.1 Garbage collection

The leading gateway runs it once a day (main spec §11.2 and §11.3). It considers index entries whose `lastUsed` is older than `images.unusedRetention` (default 30 days). It keeps any that an existing Intent's `status.plan` still references, or one of its ManagedResources' probes.
- **The references are only a hint.** Intent status is operator-written, so it is untrusted. A false reference only keeps an image longer; a missing one only causes a rebuild.

**The steps:**
1. Write `image.gc.start`.
2. Pause new builds (they queue) and wait for running ones.
3. For each candidate:
   - delete the manifest through the front proxy with the admin credential;
   - drop it from the index;
   - write `image.deleted`.
4. `POST` to the GC sidecar, with the admin credential. The sidecar is `images/registry-gc`: the `registry` binary plus a small Go HTTP server, reachable only from the gateway by NetworkPolicy. It runs `registry garbage-collect` on the shared PVC and returns the bytes freed.
5. Resume builds, then write `image.gc.result` with the bytes freed.

Pulls continue throughout. A sidecar failure is recorded in `image.gc.result`, and builds resume anyway.

### 6.2 External registry mode

`registry.mode: external` with `repository`, `pushSecretRef` and `pullSecretRef`, each a `kubernetes.io/dockerconfigjson` Secret in `last-gateway`.
- **Builds** push with the push secret, and their egress must include the registry's CIDRs (D5).
- **Sandbox pods.** The gateway copies the pull secret into `last-sandbox` as `last-registry-pull` and lists it in `imagePullSecrets`.
- **GC** deletes manifests with the push secret. Where the registry refuses `DELETE` (405, or `UNSUPPORTED`; ECR and GHCR among them), `image.deleted` records `registryDeleted: false` and the entry still leaves the index. The registry's own lifecycle policy then reclaims storage *(my call)*.
- **No sidecar.** The chart skips the bundled registry entirely.

## 7. Errors

| Condition | Result |
|---|---|
| `image.install` with no digest-pinned base | `FailedPrecondition`: "image builds need OperatorConfig.sandbox.baseImage pinned by digest" |
| A hostname in `images.buildEgress` | `FailedPrecondition` naming the entry; the gateway also logs it at startup |
| The registry is unreachable during a lookup or a push | `Unavailable` (the operator retries) |
| The build fails or times out | A command or probe failure with the reason `ImageBuildFailed` and the log tail (§4.3) |
| The indexed build entry fails verification | The entry leaves the index, a rebuild follows, and the gateway logs at error level |
| A pull failure that outlasts the grace period | A command failure, as today, with the pod's waiting reason |
| A GC run while the sidecar is down | `image.gc.result` records the error, and builds resume |

## 8. Security

- **Build pods** get no ServiceAccount token, no provider credentials, the pusher credential only, limited egress plus DNS, and resource and time limits. They run in a privileged-labelled namespace, with relaxed seccomp and AppArmor and no process sandbox (D3, D4).
- **Install steps can read the pusher credential.** It is in the build container's environment and filesystem, and with no process sandbox, a `RUN` step can read it.
  - **What that allows:** a malicious install step can push arbitrary content under any tag, which costs storage.
  - **What it cannot do:** affect the images of other specs or anything outside its own image, since only audited digests run; or delete anything, since the pusher cannot DELETE (spike finding 4). An install step decides its own image's contents anyway, and without a process sandbox it can influence which of its own pushed digests the build result records.
  - This is new in main spec §17.2.
- **The pull proxy is anonymous but narrow.** It allows reads of `last/images` and `last/base` by digest only. Images never contain credentials, because builds never receive any.
- **The admin credential** stays in `last-gateway`. The operator cannot read `last-registry`, `last-build` or `last-gateway`.
- **The index is a cache.** Every digest that runs was named by a signed `image.build.result`, checked on first use (D2).

## 9. Testing

**Unit tests:**
- spec hashing, with golden vectors;
- Containerfile generation, including argv elements that need JSON escaping;
- the build NetworkPolicy and pod;
- index verification, including tampered, missing and mismatched entries;
- single flight, including the failure fan-out;
- pull-grace classification;
- turn-schema strictness and the shared-object test;
- CLI rendering of the install commands.

**envtest:**
- the gateway's resolve paths against a fake registry (`go-containerregistry`'s in-memory registry);
- build objects created and cleaned up;
- the build wait queue during GC.

**Helm template tests:**
- the front proxy's method map;
- the pull proxy's path allowlist;
- the NetworkPolicies.

**e2e on kind:**
- `hack/registry-check.sh` passes on a two-node cluster.
- **The done-criterion.** The scripted model plans a command whose install needs no network. It writes a small script to `/usr/local/bin/last-e2e-tool` with `python3`, so the e2e does not depend on package mirrors. The command runs it, and so does a probe. The test then asserts:
  - exactly one `image.build.start` for the spec hash across the command, its probes and a second Intent with the same spec;
  - every `command.start` and `probe.start` names the same digest;
  - `audit verify` passes.
- **A failed build** (`install: [["false"]]`) fails the step with `ImageBuildFailed`.
- **M4c:** a GC run with `unusedRetention` set to seconds deletes an unused image, keeps a used one, and frees bytes.

**Opt-in demo:** `make demo` gains a request needing `awscli`. Its build uses real Debian mirrors with `buildEgress: ["0.0.0.0/0:80", "0.0.0.0/0:443"]`.

## 10. Main-spec amendments

- **§5.6 and §6:** `images.buildEgress` takes only IP and CIDR entries and defaults to empty (D5). `images` gains `buildTimeout` and `maxConcurrentBuilds`.
- **§11.1:** pull failures get a 60-second grace period (D8). Builds need a digest-pinned base, which may live in the bundled registry (D6).
- **§11.2:**
  - step 1 becomes the persisted, audit-checked index (D2);
  - build pods run in `last-build` (D3), without the process sandbox (D4), with DNS;
  - builds happen inside `RunCommand` and `RunProbes` (D7);
  - failure handling follows §4.3.
- **§11.3:**
  - the pull proxy's path allowlist;
  - the front proxy's per-method map;
  - the IPv4-only hostPort;
  - `hack/registry-check.sh` as the validation for each platform;
  - external `DELETE` that the registry does not support.
- **§17.2:**
  - install steps can read the pusher credential;
  - bundled mode is unverified on IPv6-first nodes;
  - the e2e install avoids the network.
- **§19:** `internal/images` and `images/registry-gc` as already listed. `last-build` joins the namespace table in §3.

## 11. Left to the builder

- Exact image versions: Distribution 3.x, nginx 1.27.x, BuildKit v0.26.x-rootless. Pin exact tags.
- How the build wrapper prints its result line, and how the gateway parses it, in the style of the runner's `LAST-RESULT` line.
- How the egress gate reaches the build pod. The BuildKit image does not contain our runner, so one way is an init container that copies `sandbox-runner` into an emptyDir, with the runner then exec'ing `buildctl-daemonless.sh`.
- Whether the in-memory registry in tests needs a fake for the `Head` cache clock.
- The task order inside each plan.
