# M4a (bundled registry) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-17-m4a-bundled-registry.md`.

## Result

**The full e2e passed** on a two-node kind cluster (`last-m4a`, kubelet v1.37.0, kind v0.33.0): `ok test/e2e 375s`.
- **Before the suite:** `hack/e2e.sh` runs `make nginx-check`, then `hack/registry-check.sh`. Every check passed on both nodes, including the pull proxy refusing a digest followed by `%0a`.
- **During the suite:** every sandbox pod pulled the pinned base, `localhost:5000/last/base@sha256:…`, through its node's pull proxy.

**One e2e failure on the way, now fixed.** It was an interaction with kubelet's pull-credential verification:
- **The feature:** `KubeletEnsureSecretPulledImages`, with the default policy `NeverVerifyPreloadedImages`.
- **What happened:** once the worker had pulled the base as `localhost:5000/last/base`, kubelet stopped trusting the same image ID under its kind-loaded name, `ghcr.io/moogie-labs/last-sandbox-base:e2e`. It tried to pull that name from ghcr.io and got a 403.
- **The fix:** the sandbox e2e now uses the one pinned name. Main spec §17.2 records the limitation.

## Before M4b builds on this

- **Run `hack/registry-check.sh` on EKS and GKE Standard** when clusters are available, and record the results in the spike doc (design D1).
- **The `--from` path of `hack/registrypush` is untested.** No unit test covers it, and e2e always uses `--image-tar`, yet it is the default path the README gives new-platform users. Add a test that pushes to the in-memory registry and pulls back with `--from`.
- **`hack/registry-check.sh` hard-codes names the chart makes configurable:**
  - the namespaces `last-registry` and `last-gateway`;
  - `localhost:5000`, which follows `registry.bundled.hostPort`.

  Add flags or read them from the cluster, or drop the `hostPort` value until M4b uses it.
- **The registry check does not test NetworkPolicy enforcement.** Its requests go through port-forwards, which bypass policy. Add a curl pod in the check namespace that must fail to reach `last-registry-front` and `last-registry`. This matters on platforms that may not enforce policy, such as EKS with VPC CNI policy enforcement off.
- **`last-sandbox` has no Pod Security label.** Design D3 says the namespace "keeps enforcing restricted", but the chart never labelled it. This predates M4a. Decide and label it.

## Registry check

- **Each run leaves its image behind.** It is a `registry-check` tag in `last/images`, and M4c's index-driven GC will never delete it. Delete it at the end with the admin credential, which also adds a positive admin-DELETE check.
- **Clusters with nodes of more than one architecture fail falsely.** The image's platform comes from the first node.
- **Unschedulable nodes are included.** NotReady, virtual and Fargate nodes are all checked, although design §3.5 says "schedulable".
- **The check namespace has no Pod Security label.** On clusters that enforce `restricted` by default, the busybox pod is rejected and the script stops without a FAIL line.

## Chart

- **The front proxy buffers responses** (nginx's default). Large blob GETs can spill to `/tmp` when the reader is slow. Consider `proxy_buffering off`, as the pull proxy already has.
- **No PriorityClass for the pull proxy.** It now has resource requests, but no priority class.
- **The front proxy restarts on every upgrade.** `checksum/auth` hashes the re-salted htpasswd Secret. The restart isn't needed for correctness, because nginx reads `auth_basic_user_file` per request. It is harmless, since the rollout has no downtime, but it can cut a push that is still running after 30 s. Hashing only the plaintext passwords would restart the proxy only on a real credential change.
- **No front-proxy liveness probe.**
- **Some `helm-lint` greps are looser than they read.** The `bundled=false` check greps for `namespace: last-registry`, and the PSA check passes if either namespace has the label.
- **Main spec §11.3 states the front proxy's method rules twice.**

## Tests

- **`test/admission/operator_rbac_test.go` doesn't list the new namespaces.** Add `last-registry` and `last-build` as regression pins for the operator's lack of access. The cluster-wide denial covers them today.
- **`internal/images`:**
  - `TestPusherCannotDelete` only checks `err != nil`. Pin the 401 through `transport.Error`.
  - The password-leak check in `TestHeadReportsAuthFailures` is weak, because the server never echoes the password.
  - Two tests discard `random.Image` errors.
- **`hack/registrypush`:** the label test checks only that the digest changed, and the comment in `TestNeverPrintsThePassword` is muddled.
- **Sandbox executor:** the grace-reset path (a pull failure that clears and then recurs) is untested. `TestRunFailsFastOnImagePullBackOff` now waits out a 20 ms grace, so its name is slightly off.

## Scripts

- **`hack/kind-install.sh`:**
  - The port-forward readiness loop times out silently; the failure then shows up as a `registrypush` connection error.
  - `BASE_DIGEST` is not validated before it is applied.
  - The OperatorConfig server-side apply has no `--force-conflicts`.
  - The image pins are duplicated between the script and `values.yaml`.
  - A reused one-node cluster silently loses two-node coverage.
  - A local variable named `tar` shadows the command.
- **The spike doc's setup line says Kubernetes v1.36.** The same kind version produced v1.37.0 nodes in M4a, so check the version and correct the line.
