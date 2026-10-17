# M4a: Bundled Registry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Install a bundled OCI registry with the chart. Kubelet on every node pulls `localhost:5000/last/…@sha256:…` through a node-local proxy, and e2e sandbox pods already run the base image from it.

**Architecture:**
- **The chart** adds Distribution v3 (with a PVC), an nginx front proxy that picks the credential by HTTP method, and an nginx DaemonSet on `hostPort` 5000 that allows only pulls by digest. They live in `last-registry`, along with the `last-build` namespace that M4b fills.
- **Go code:**
  - `internal/images`, a small registry client on go-containerregistry;
  - `hack/registrypush`, a tool that pushes an image through it;
  - a pull-failure grace period in the sandbox executor.
- **Scripts:** the kind install pushes the base image into the registry and pins `OperatorConfig.sandbox.baseImage` to it. `hack/registry-check.sh` validates the whole path on any cluster and runs before the e2e suite.

**Tech Stack:** Helm templates, nginx (nginx-unprivileged), Distribution v3, Go 1.26, `github.com/google/go-containerregistry` v0.20.3, kind, bash.

**Spec:** `docs/superpowers/specs/2026-10-17-m4-image-builds-design.md`:
- §3 and §9 (the M4a parts);
- D1, D3, D6 and D8;
- the §10 amendments for §3, §11.1 and §11.3.

The evidence is in `docs/superpowers/spikes/2026-10-17-m4-registry-spike.md`. The parent spec is `docs/superpowers/specs/2026-09-26-last-operator-design.md`.

## Global Constraints

- **Commits:** never add a commit attribution trailer (such as `Co-authored-by:`). Never commit `.superpowers/`.
- **Credentials:** a registry password never appears in logs, errors, command lines or stdout. Pass it through the environment or a file, and never as a flag.
- **Images, pinned exactly:**
  - `registry:3.0.0` (Distribution);
  - `nginxinc/nginx-unprivileged:1.27.5-alpine` (both proxies);
  - `docker.io/library/busybox:1.37.0` (the default registry-check image).
- **Go module:** `github.com/google/go-containerregistry v0.20.3`.
- **Names:**

  | Thing | Value |
  |---|---|
  | Distribution Service | `last-registry`, port 5000 |
  | Front proxy Service | `last-registry-front`, port 5000 |
  | Pull-proxy DaemonSet | `last-registry-pull-proxy`, `hostPort` 5000 |
  | Pod label `app.kubernetes.io/name` | the same as each object's name |
  | Repositories | `last/images`, `last/base` |
  | Front proxy host for in-cluster clients | `last-registry-front.<namespaces.registry>.svc:5000` |
  | Kubelet's host | `localhost:5000` |
- **Secrets:**
  - In `namespaces.gateway`: `last-registry-pusher` and `last-registry-admin`, each with keys `username` (`pusher` / `admin`) and `password`. These are where `last.persistedSecretValue` reads from.
  - In `namespaces.registry`: `last-registry-auth`, with keys `pusher` and `admin`, each one htpasswd line from Helm's `htpasswd` (bcrypt; verified with nginx alpine during planning: right password 200, wrong 401).
- **Namespaces:**
  - `namespaces.registry` (default `last-registry`) and `namespaces.build` (default `last-build`), both labelled `pod-security.kubernetes.io/enforce: privileged`;
  - `last-registry` is rendered only when `registry.bundled.enabled`;
  - `last-sandbox` is unchanged.
- **The front proxy's method rules** (spec D2, spike finding 4): GET and HEAD anonymous; DELETE admin only; every other method pusher only.
- **The pull proxy's allowlist** (spike finding 5): GET and HEAD only, on:
  - `^/v2/$`;
  - `^/v2/last/(images|base)/(manifests|blobs)/sha256:[0-9a-f]{64}$`;
  - a local `/healthz`.

  Everything else gets 403.
- **Pull grace** (D8): 60 s by default. It applies to `ErrImagePull` and `ImagePullBackOff` only. `InvalidImageName`, `CreateContainerConfigError` and `CreateContainerError` still fail at once.
- **After Go changes:** run `make lint-fix`, `make lint` and `make test`. **After chart changes:** run `make helm-lint`.

## Review Focus

1. **A `helm upgrade` must keep both registry passwords.** `htpasswd` re-salts on every render, so the front proxy must restart when `last-registry-auth` changes, and still accept the unchanged passwords. Task 3 pins it with a checksum-annotation check in `helm-lint`; Task 6 reruns the install on the same cluster.
2. **A pull through a proxy that is just restarting must not fail the command.** Task 5 tests a backoff that clears inside the grace period.
3. **Requests near the allowlist must be refused:** tag lists, `_catalog`, a different repository, a tag instead of a digest, and write methods. Task 7's registry check asserts each through a real pull proxy.
4. **`registry.bundled.enabled=false` must render no `last-registry` objects.** `last-build` and the gateway's Role there still render. Task 3 adds the render check.
5. **Pushes that hit the wrong credential must be refused:** an anonymous push, a pusher DELETE, an admin PUT. Task 7 asserts the full method matrix against the front proxy.

---

### Task 1: `internal/images` registry client

**Files:**
- Create: `internal/images/registry.go`
- Create: `internal/images/registry_test.go`
- Modify: `go.mod`, `go.sum` (add `github.com/google/go-containerregistry v0.20.3`)

**Interfaces:**
- Produces:

  ```go
  package images

  // Credentials authenticate to the front proxy (spec §11.3).
  type Credentials struct{ Username, Password string }

  // Client talks to one registry host such as "localhost:15000" or
  // "last-registry-front.last-registry.svc:5000". insecure selects plain HTTP.
  func NewClient(host string, creds *Credentials, insecure bool) *Client

  // Head reports whether repo has a manifest with this digest ("sha256:<64 hex>").
  // 404 is (false, nil); any other failure is an error.
  func (c *Client) Head(ctx context.Context, repo, digest string) (bool, error)

  // Delete deletes the manifest; deleting one that does not exist is not an error.
  func (c *Client) Delete(ctx context.Context, repo, digest string) error

  // Push writes img as repo:tag and returns its manifest digest.
  func (c *Client) Push(ctx context.Context, repo, tag string, img v1.Image) (string, error)
  ```

  Here `v1` is `github.com/google/go-containerregistry/pkg/v1`. Errors never contain the password; go-containerregistry does not put it in error text, and the test pins that.

- [ ] **Step 1: Write the failing tests**

  Serve `github.com/google/go-containerregistry/pkg/registry.New()` with `httptest`, wrapped in a basic-auth middleware that copies the front proxy's rules (Global Constraints). Push `random.Image(1024, 1)` from `pkg/v1/random`. Tests:
  - `TestPushThenHeadFindsTheManifest`: `Push` returns `img.Digest().String()`, and `Head` returns `true`.
  - `TestHeadOfAMissingDigestIsFalse`: `(false, nil)` for `"sha256:" + 64 zeros`.
  - `TestDeleteRemovesTheManifest`: as admin, `Delete` and then `Head` gives `false`. A second `Delete` returns nil.
  - `TestPusherCannotDelete`: as pusher, `Delete` returns an error.
  - `TestHeadReportsAuthFailures`: with a middleware that answers 401 to every request, `Head` returns an error, not `false`. The error string does not contain the password `"s3cret-pw"`.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/images/`. Expected: FAIL, because the package does not exist yet.

- [ ] **Step 3: Implement `registry.go`**

  - Use `name.NewDigest` and `name.NewTag`, with `name.Insecure` when `insecure` is set.
  - Use `remote.Head`, `remote.Delete` and `remote.Write`, with `remote.WithAuth(&authn.Basic{…})` (anonymous when `creds` is nil) and `remote.WithContext(ctx)`.
  - Map a 404 to `false` or nil with `transport.Error`'s `StatusCode`.
  - Run `go get github.com/google/go-containerregistry@v0.20.3` and `go mod tidy`.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/images/ && make lint`. Expected: PASS, 0 issues.

- [ ] **Step 5: Commit**

  ```bash
  git add go.mod go.sum internal/images
  git commit -m "feat(images): add the registry client for the bundled and external registries"
  ```

---

### Task 2: `hack/registrypush`

A tool the scripts use to push an image through the front proxy. It is the client's first consumer.

**Files:**
- Create: `hack/registrypush/main.go`
- Create: `hack/registrypush/main_test.go`

**Interfaces:**
- Consumes: `images.NewClient`, `(*Client).Push` (Task 1).
- Produces the command line:

  ```
  go run ./hack/registrypush --to HOST/REPO:TAG (--tar FILE | --from REF [--platform OS/ARCH]) [--label KEY=VALUE]
  ```

  - The credentials come from `LAST_REGISTRY_USERNAME` and `LAST_REGISTRY_PASSWORD`; both empty means anonymous.
  - It uses plain HTTP when HOST starts with `localhost:` or `127.0.0.1:`.
  - It prints only the pushed digest (`sha256:…`) and a newline to stdout. Errors go to stderr, with exit status 1.
  - `--tar` reads a `docker save` archive with `tarball.ImageFromPath(file, nil)`.
  - `--from` pulls one platform's image with `remote.Image(ref, remote.WithPlatform(…))`. The default platform is `linux/amd64`.
  - `--label` sets one config label with `mutate.Config`. The registry check uses it to make a digest no node has cached.
- Testable entry point: `func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int`.

- [ ] **Step 1: Write the failing tests**

  These use the same in-memory registry and auth middleware as Task 1. Copy the small helper; don't export it from `internal/images`.
  - `TestPushesATarballAndPrintsTheDigest`: write `random.Image` with `tarball.WriteToFile`, then run with `--tar` and pusher credentials from `getenv`. Stdout is exactly `<digest>\n` and the exit status is 0.
  - `TestALabelChangesTheDigest`: the same tarball pushed with `--label last.check=abc` prints a different digest from the unlabelled push.
  - `TestRefusesBothOrNeitherSource`: `--tar` with `--from`, and neither, each exit with 1 and a usage message on stderr.
  - `TestNeverPrintsThePassword`: an anonymous push to the auth middleware fails, and neither stream contains the password.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./hack/registrypush/`. Expected: FAIL (it does not compile yet).

- [ ] **Step 3: Implement `main.go`**

  `main` calls `os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))`. Parse with a fresh `flag.FlagSet` and `ContinueOnError`.

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./hack/registrypush/ && make lint`. Expected: PASS, 0 issues.

- [ ] **Step 5: Commit**

  ```bash
  git add hack/registrypush
  git commit -m "feat(hack): add registrypush to push images through the registry front proxy"
  ```

---

### Task 3: Chart — Distribution, front proxy and credentials

**Files:**
- Modify: `charts/last-operator/values.yaml`
- Modify: `charts/last-operator/templates/namespaces.yaml`
- Create: `charts/last-operator/templates/registry-secrets.yaml`
- Create: `charts/last-operator/templates/registry.yaml`
- Modify: `Makefile` (`helm-lint` target)
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §11.3 (the front proxy's method map)

**Interfaces:**
- Produces these values, used by Tasks 4, 6 and 7:

  ```yaml
  namespaces:
    registry: last-registry
    build: last-build
  registry:
    bundled:
      enabled: true
      image: registry:3.0.0
      proxyImage: nginxinc/nginx-unprivileged:1.27.5-alpine
      storageClassName: ""   # "" = cluster default
      size: 20Gi
      hostPort: 5000
      resources:
        requests: {cpu: 50m, memory: 64Mi}
        limits: {memory: 512Mi}
  ```

  It also produces the Services `last-registry` and `last-registry-front`, and the Secrets named in Global Constraints.

- [ ] **Step 1: Add the failing render checks to `helm-lint`**

  Use the existing grep/awk style:
  - The default render contains `name: last-registry-front`, `name: last-registry-auth`, and `pod-security.kubernetes.io/enforce: privileged`.
  - The front proxy's ConfigMap contains `map $request_method $realm` and `DELETE /etc/nginx/auth/admin`.
  - The front Deployment's pod template has a `checksum/auth:` annotation (Review Focus 1).
  - Rendering with `--set registry.bundled.enabled=false` contains no `namespace: last-registry` (Review Focus 4).

- [ ] **Step 2: Run the checks and confirm they fail**

  Run `make helm-lint`. Expected: FAIL on the first new grep.

- [ ] **Step 3: Write the templates**

  - **`namespaces.yaml`:** add `namespaces.registry`, only when `registry.bundled.enabled`, with the privileged PSA label and `helm.sh/resource-policy: keep` (it holds the PVC). Add `namespaces.build`, always, with the privileged PSA label.
  - **`registry-secrets.yaml`:**
    - `last-registry-pusher` and `last-registry-admin` in the gateway namespace. Passwords come from `last.persistedSecretValue (list $gw "last-registry-pusher" "password")`, and the same for admin, kept with `helm.sh/resource-policy: keep`.
    - `last-registry-auth` in the registry namespace, from `htpasswd "pusher" $pusher` and `htpasswd "admin" $admin`.
    - All of it only when bundled.
  - **`registry.yaml`:**
    - **The PVC** `last-registry-data`, ReadWriteOnce, size and storage class from values, with `helm.sh/resource-policy: keep`.
    - **The Deployment `last-registry`:**
      - one replica, strategy `Recreate`, image from values;
      - env `REGISTRY_STORAGE_DELETE_ENABLED=true`, the data mounted at `/var/lib/registry`;
      - the RustFS template's pod security context, with UID and GID 65532;
      - a readiness probe of `GET /v2/` on 5000.
    - **The Service `last-registry`.**
    - **The front proxy:** a ConfigMap `last-registry-front` holding the spike's `default.conf` (spike finding 4), with `proxy_pass` to `http://last-registry.<ns>.svc:5000`. Then a Deployment `last-registry-front`:
      - one replica, image `proxyImage`;
      - `last-registry-auth` mounted at `/etc/nginx/auth`, so the files are `/etc/nginx/auth/pusher` and `/etc/nginx/auth/admin`;
      - the annotation `checksum/auth: {{ include (print $.Template.BasePath "/registry-secrets.yaml") . | sha256sum }}`;
      - non-root, with all capabilities dropped, as in the RustFS template;
      - a readiness probe of `GET /v2/`.
      - Then the Service `last-registry-front`.
    - **NetworkPolicy `last-registry`:** it selects the Distribution pod, and allows ingress on 5000 only from pods labelled `app.kubernetes.io/name: last-registry-front`.
    - **NetworkPolicy `last-registry-front`:** ingress on 5000 from:
      - the pull-proxy pods;
      - the namespace `namespaces.build`;
      - pods `app.kubernetes.io/name: last-gateway` in `namespaces.gateway`.
  - **Spec §11.3:** add the method map and "credentials stored as bcrypt htpasswd lines; the passwords survive upgrades".

- [ ] **Step 4: Run the checks**

  Run `make helm-lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add charts/last-operator Makefile docs/superpowers/specs/2026-09-26-last-operator-design.md
  git commit -m "feat(chart): bundle Distribution behind an nginx front proxy with per-method credentials"
  ```

---

### Task 4: Chart — node-local pull proxy and the `last-build` namespace

**Files:**
- Modify: `charts/last-operator/templates/registry.yaml` (the pull proxy)
- Create: `charts/last-operator/templates/build.yaml`
- Modify: `Makefile` (`helm-lint`)
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md`:
  - §3, the namespace table: add `last-build` (build pods, privileged PSA, written only by the gateway);
  - §11.3: the path allowlist, the IPv4-only hostPort, the pull proxy having no NetworkPolicy and why.

**Interfaces:**
- Consumes: the values and the `last-registry-front` Service from Task 3.
- Produces:
  - the DaemonSet `last-registry-pull-proxy`, serving `localhost:5000` on every node;
  - in `namespaces.build`: the NetworkPolicy `last-build-default-deny`, and the Role and RoleBinding `last-gateway-build` for the gateway's ServiceAccount, which M4b uses.

- [ ] **Step 1: Add the failing render checks to `helm-lint`**

  - The render contains `hostPort: 5000` and `name: last-build-default-deny`.
  - The pull proxy's ConfigMap contains `^/v2/last/(images|base)/(manifests|blobs)/sha256:[0-9a-f]{64}$` and `return 403`.
  - The `last-gateway-build` Role grants `configmaps` the verbs `create, list, delete`.

- [ ] **Step 2: Run the checks and confirm they fail**

  Run `make helm-lint`. Expected: FAIL.

- [ ] **Step 3: Write the templates**

  - **The pull-proxy ConfigMap `last-registry-pull-proxy`:**

    ```nginx
    server {
      listen 5000;
      location = /healthz { access_log off; return 200; }
      location = /v2/ { limit_except GET { deny all; } proxy_pass http://last-registry-front.<ns>.svc:5000; }
      location ~ ^/v2/last/(images|base)/(manifests|blobs)/sha256:[0-9a-f]{64}$ {
        limit_except GET { deny all; }
        proxy_pass http://last-registry-front.<ns>.svc:5000;
        proxy_set_header Host $http_host;
        proxy_buffering off;
      }
      location / { return 403; }
    }
    ```

    `limit_except GET` also allows HEAD.
  - **The DaemonSet `last-registry-pull-proxy`:**
    - image `proxyImage`, `containerPort` 5000 with `hostPort: {{ .Values.registry.bundled.hostPort }}`;
    - `tolerations: [{operator: Exists}]`;
    - readiness and liveness probes on `/healthz`;
    - non-root, with all capabilities dropped.

    It gets no NetworkPolicy, because kubelet reaches it from the node through the hostPort DNAT, and how CNIs treat that traffic differs. Add a comment saying so.
  - **`build.yaml`:**
    - `last-build-default-deny`, with `podSelector: {}` and both Ingress and Egress policy types;
    - the Role `last-gateway-build`:
      - `pods`: create, get, list, delete;
      - `pods/log`: get;
      - `secrets`, `configmaps` and `networkpolicies`: create, list, delete;
    - the RoleBinding to `last-gateway` in `namespaces.gateway`.
  - **Spec edits** as listed under Files.

- [ ] **Step 4: Run the checks**

  Run `make helm-lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add charts/last-operator Makefile docs/superpowers/specs/2026-09-26-last-operator-design.md
  git commit -m "feat(chart): add the node-local pull proxy and the last-build namespace"
  ```

---

### Task 5: Executor pull grace

**Files:**
- Modify: `internal/sandbox/executor.go`, around lines 60–80 (config) and 360–395 (`await`)
- Modify: `internal/sandbox/executor_test.go` (`newExecWith`, plus the new tests)
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §11.1 (the pull-grace sentence)

**Interfaces:**
- Produces `ExecutorConfig.PullGrace time.Duration`: how long `ErrImagePull` or `ImagePullBackOff` may last before the run fails. It defaults to 60 s in `withDefaults`. The gateway keeps the default and wires nothing.

- [ ] **Step 1: Write the failing tests**

  First set `PullGrace: 20 * time.Millisecond` in `newExecWith`, so `TestRunFailsFastOnImagePullBackOff` keeps its meaning. Then add:
  - `TestRunWaitsOutAPullBackOffWithinTheGrace`:
    - a reactor on `get pods` reports `ImagePullBackOff` for the first 5 gets, then `PodSucceeded`;
    - the executor is built with `PullGrace: 5 * time.Second`;
    - `Run` returns the envelope's result with no error.
  - `TestRunFailsAPullBackOffThatOutlastsTheGrace`: `PullGrace: 200 * time.Millisecond`. The error contains `ImagePullBackOff`, and the elapsed time is ≥ 200 ms and < 5 s.
  - `TestRunFailsFastOnInvalidImageNameDespiteTheGrace`: `PullGrace: time.Minute`, with the reason `InvalidImageName`. It fails in under 1 s.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/sandbox/ -run 'PullBackOff|InvalidImageName' -v`. Expected: the first new test FAILS, because today's code fails at once.

- [ ] **Step 3: Implement**

  In `await`, record when an `ErrImagePull` or `ImagePullBackOff` is first seen. Fail with the existing message only once `PullGrace` has passed since then. Every other reason in `failFastReasons` still fails at once.

  Edit the spec §11.1 line: "kubelet's image pull failures are retried for 60 s before the command fails (a node's pull proxy may be restarting)."

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/sandbox/ && make lint`. Expected: PASS, 0 issues.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/sandbox docs/superpowers/specs/2026-09-26-last-operator-design.md
  git commit -m "feat(sandbox): retry image pull failures for a grace period before failing the run"
  ```

---

### Task 6: kind install pins the base image in the bundled registry

**Files:**
- Create: `hack/kind-config.yaml` (one control-plane node and one worker)
- Modify: `hack/kind-install.sh`

**Interfaces:**
- Consumes: `hack/registrypush` (Task 2); the Services and Secrets from Task 3; the pull proxy from Task 4.
- Produces, after `hack/kind-install.sh`:
  - `OperatorConfig/default` has `spec.sandbox.baseImage: localhost:5000/last/base@sha256:<digest>`, set by server-side apply with `--field-manager kind-install`;
  - the cluster has two nodes when the script created it.

  Task 7 and the e2e suite rely on this.

- [ ] **Step 1: Change the script**

  - **Cluster creation:** `kind create cluster --name "$CLUSTER" --config hack/kind-config.yaml --wait 120s`. A reused cluster keeps its shape.
  - **Preloading:** factor the existing platform-tar `kind load` into a function, `load_image IMAGE`. Use it for `$NGINX_IMAGE`, `registry:3.0.0` and `nginxinc/nginx-unprivileged:1.27.5-alpine`, so the cluster never pulls these from a public registry.
  - **After `helm upgrade --install … --wait`:**
    1. `docker save` the sandbox base image to a temporary file.
    2. Start `kubectl port-forward -n last-registry svc/last-registry-front 15000:5000` in the background, and kill it and `wait` for it in the EXIT trap (as `hack/demo.sh` does).
    3. Read the pusher password with `kubectl get secret -n last-gateway last-registry-pusher -o jsonpath='{.data.password}' | base64 --decode` into a variable, never echoed.
    4. Run `LAST_REGISTRY_USERNAME=pusher LAST_REGISTRY_PASSWORD=… go run ./hack/registrypush --to localhost:15000/last/base:"$TAG" --tar "$BASE_TAR"`, and capture the digest.
    5. Server-side apply the OperatorConfig `default` with only `spec.sandbox.baseImage`.

- [ ] **Step 2: Run it on a fresh cluster**

  Run `KIND_CLUSTER=last-m4a hack/kind-install.sh`. Expected:
  - it exits 0;
  - `kubectl get operatorconfig default -o jsonpath='{.spec.sandbox.baseImage}'` prints `localhost:5000/last/base@sha256:…`;
  - `kubectl get ds -n last-registry last-registry-pull-proxy` shows 2 of 2 ready.

- [ ] **Step 3: Run it again on the same cluster** (Review Focus 1)

  Run `KIND_CLUSTER=last-m4a hack/kind-install.sh`. Expected: it exits 0, the base push succeeds with the same pusher password, and the digest is unchanged.

- [ ] **Step 4: Commit**

  ```bash
  git add hack/kind-config.yaml hack/kind-install.sh
  git commit -m "feat(hack): run e2e on two nodes with the sandbox base served from the bundled registry"
  ```

---

### Task 7: `hack/registry-check.sh`, wired into the e2e

**Files:**
- Create: `hack/registry-check.sh`
- Modify: `hack/e2e.sh`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §11.3 ("Validation required" now names `hack/registry-check.sh`)
- Modify: `README.md` (one paragraph under installation: running the check on a new platform)

**Interfaces:**
- Consumes: `hack/registrypush` (Task 2) and the chart objects (Tasks 3 and 4).
- Produces the command line:

  ```
  hack/registry-check.sh [--context CTX] [--image-tar FILE]
  ```

  It uses the current kube context unless `--context` is given. It exits 0 only if every check passes, and prints one line per check: `PASS|FAIL <check>[: detail]`.

- [ ] **Step 1: Write the script**

  Write it in bash with `set -euo pipefail`. Use a `kc` function for kubectl with the context; zsh-style variable command words don't work. Use a cleanup trap that kills port-forwards with `wait` and deletes the namespace `last-registry-check`. The steps:

  1. **Push a fresh image.** Port-forward `svc/last-registry-front` to 15000, and read the pusher password as in Task 6. Push to `localhost:15000/last/images:registry-check` with `--label last.check=<random hex>`, so no node has the digest cached:
     - from `--image-tar`;
     - or, by default, `--from docker.io/library/busybox:1.37.0 --platform linux/<arch of the first node>`.
  2. **Pull on every node.** For every node, create a pod in `last-registry-check` with:
     - `nodeName`, `tolerations: [{operator: Exists}]`, `restartPolicy: Never`;
     - image `localhost:5000/last/images@<digest>`;
     - `command: ["true"]`.

     Wait up to 120 s for `Succeeded`. Record `PASS pull on <node>`, or `FAIL pull on <node>: <waiting reason>`.
  3. **Confirm it went through the proxy.** For each node, the pull-proxy pod on it (`--field-selector spec.nodeName=<node>`) logged `GET /v2/last/images/manifests/<digest>`. Record `PASS proxy on <node>`.
  4. **The pull proxy's refusals.** Port-forward one pull-proxy pod to 15001. Each must answer 403:
     - `PUT`, `POST`, `PATCH` and `DELETE` on `/v2/last/images/manifests/<digest>`;
     - `GET /v2/_catalog`;
     - `GET /v2/last/images/tags/list`;
     - `GET /v2/other/manifests/<digest>`;
     - `GET /v2/last/images/manifests/registry-check` (a tag).

     `GET /v2/last/images/manifests/<digest>` must answer 200.
  5. **The front proxy's method matrix** on 15000, with the manifest Accept header for GETs:

     | Request | Expected |
     |---|---|
     | anonymous GET | 200 |
     | anonymous HEAD | 200 |
     | anonymous PUT | 401 |
     | anonymous DELETE | 401 |
     | pusher DELETE | 401 |
     | admin PUT | 401 |
     | admin DELETE of `sha256:` + 64 zeros | 404 |

     Read the admin password like the pusher's. Pass both to curl through a `--config` file in a `mktemp` directory, so neither appears on a command line.
  6. **Print a summary,** and exit 1 if any line is FAIL.

- [ ] **Step 2: Wire it into the e2e**

  In `hack/e2e.sh`, after `hack/kind-install.sh` and before `go test`: `docker save` the sandbox base to a temporary file, then run `hack/registry-check.sh --image-tar "$TMP"`. A failed check fails the e2e.

- [ ] **Step 3: Run the full e2e on the Task 6 cluster**

  Run `KIND_CLUSTER=last-m4a hack/e2e.sh 2>&1 | tee "$SCRATCH/m4a-e2e.log"`. Expected:
  - every registry-check line is PASS, for both nodes;
  - `ok … test/e2e`. Every existing e2e test now runs its sandbox pods from `localhost:5000/last/base@…`.

- [ ] **Step 4: Edit the docs**

  - **Spec §11.3, "Validation required":** "run `hack/registry-check.sh` against the platform; kind and k3s pass (spike, 2026-10-17); EKS and GKE Standard are pending a run".
  - **README:** add a short paragraph on running the check.

- [ ] **Step 5: Commit**

  ```bash
  git add hack/registry-check.sh hack/e2e.sh README.md docs/superpowers/specs/2026-09-26-last-operator-design.md
  git commit -m "feat(hack): add a registry check for any cluster and run it before the e2e suite"
  ```
