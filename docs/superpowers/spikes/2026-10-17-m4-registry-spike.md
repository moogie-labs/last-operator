# M4 spike: node-local pull proxy and rootless builds (2026-10-17)

Main spec §11.3 says M4 starts by validating the bundled registry's node-local pull proxy on kind, k3s, EKS and GKE Standard. This spike covered kind and k3s, and also tried the riskiest part of §11.2: a rootless BuildKit build pod. EKS and GKE Standard are still open (see "Not covered").

The manifests were throwaway and are not in the repo. The configurations that worked are quoted below.

## Setup

- **kind** v0.33.0, Kubernetes v1.36, two nodes (control plane and worker), kindnet with the portmap plugin, containerd 2.3.4.
- **k3s** v1.36.1+k3s1 in Docker, one node, flannel, containerd 2.2.3-k3s.
- **The stack:**
  - Distribution `registry:3`, with `REGISTRY_STORAGE_DELETE_ENABLED=true`, on an emptyDir;
  - an nginx 1.27 front proxy;
  - an nginx 1.27 DaemonSet on `hostPort` 5000, the pull proxy.

  All three ran in `last-registry`, labelled `pod-security.kubernetes.io/enforce: privileged`, because the baseline level forbids `hostPort`.
- **The image:** the sandbox base image, pushed with crane through a port-forward to the front proxy: `last/images@sha256:8578cff8…`.

## Results

| Check | kind | k3s |
|---|---|---|
| kubelet pulls `localhost:5000/last/images@sha256:…` through its own node's proxy, over plain HTTP, with no containerd config | yes, on both nodes (~2 s for 94 MB) | yes (1.7 s) |
| The pull proxy refuses `PUT`/`POST`/`PATCH`/`DELETE` | 403 for all | not repeated |
| Rootless BuildKit builds the §11.2 Containerfile (`apt-get install awscli`) and pushes it with the pusher credential | yes, in 21 s | not tried |
| The built image runs from `localhost:5000` as UID 65532 (`aws-cli/2.9.19`) | yes, on both nodes | not tried |

## Findings

1. **containerd tries HTTPS first on `localhost`, then falls back to HTTP.** It logs "host will try HTTPS first since it is configured for HTTP with a TLS configuration" and then pulls over HTTP. The fallback costs milliseconds. No `certs.d` or `config_path` entry was needed on either distribution.
2. **The hostPort is IPv4-only.** `http://[::1]:5000` did not connect. Pulls worked because containerd used `127.0.0.1`. IPv6-only or IPv6-first nodes are untested, and are a known risk for the bundled mode.
3. **A missing proxy costs one kubelet backoff.** With the worker's proxy pod just deleted, a pull of an uncached digest failed (`ErrImagePull`, then `ImagePullBackOff`). kubelet's retry succeeded 14 s later, once the DaemonSet had replaced the pod. kubelet backs off from 10 s, doubling up to 5 min. **The executor today fails a command at once on `ErrImagePull` and `ImagePullBackOff` (`internal/sandbox/executor.go:361`).** For built images, M4 must treat these as transient for a grace period.
4. **One credential file for every write method lets the pusher delete.** The first front-proxy configuration used `limit_except GET { auth_basic …; }` with a single htpasswd, and the pusher's `DELETE` of a manifest returned 202. Choosing the realm and the user file per method fixes it:

   ```nginx
   # GET/HEAD anonymous; DELETE admin only; every other method pusher only.
   map $request_method $realm { GET off; HEAD off; default "last-registry"; }
   map $request_method $users { DELETE /etc/nginx/auth/admin; default /etc/nginx/auth/pusher; }
   server {
     listen 5000;
     client_max_body_size 0;
     location /v2/ {
       auth_basic $realm;
       auth_basic_user_file $users;
       proxy_pass http://registry.last-registry.svc.cluster.local:5000;
       proxy_set_header Host $http_host;
       proxy_set_header X-Forwarded-Proto $scheme;
       proxy_read_timeout 900;
       proxy_request_buffering off;
     }
   }
   ```

   The results:

   | Request | Status |
   |---|---|
   | anonymous GET or HEAD | 200 |
   | anonymous PUT or DELETE | 401 |
   | pusher DELETE | 401 |
   | admin PUT | 401 |
   | admin DELETE of a missing digest | 404, from the registry (the request got through the proxy) |
5. **The pull proxy serves more than kubelet needs.** `/v2/_catalog` and the tag lists are readable through it. kubelet needs only `GET`/`HEAD` of `/v2/`, plus `/v2/last/images/manifests/sha256:…` and `/v2/last/images/blobs/sha256:…`. Restrict the pull proxy to those paths.
6. **The rootless BuildKit configuration that worked** (`moby/buildkit:v0.26.2-rootless`):

   ```yaml
   env:
   - {name: BUILDKITD_FLAGS, value: "--oci-worker-no-process-sandbox --config /etc/buildkit/buildkitd.toml"}
   - {name: DOCKER_CONFIG, value: /run/push}      # config.json with the pusher credential
   command: [buildctl-daemonless.sh]
   args: [build, --frontend, dockerfile.v0, --local, context=/ctx, --local, dockerfile=/ctx,
          --opt, filename=Containerfile,
          --output, "type=image,name=<front>/last/images:<tag>,push=true,registry.insecure=true",
          --metadata-file, /tmp/meta.json]
   securityContext:
     runAsUser: 1000
     runAsGroup: 1000
     seccompProfile: {type: Unconfined}
     appArmorProfile: {type: Unconfined}
   ```

   `buildkitd.toml` marks the front proxy as `http = true`, so the build can pull its `FROM` image from the bundled registry. The build log prints the pushed digest, and `--metadata-file` records it (`containerimage.digest`).
   - `seccompProfile: Unconfined` violates the **baseline** Pod Security level, so build pods cannot run in a namespace that enforces baseline or restricted.
   - **The process sandbox could not be kept.** Without `--oci-worker-no-process-sandbox`, but with `hostUsers: false` and `procMount: Unmasked`, rootlesskit failed: `newuidmap … write to uid_map failed: Operation not permitted`. A pod user namespace leaves no room for rootlesskit's nested ID map. So build steps share BuildKit's PID namespace. That matches main spec §17.2, "build pods are less isolated than sandboxes".
7. **A `FROM` image must be reachable from the build pod.** In e2e, the base image exists only in the nodes' containerd, loaded by `kind load`, and BuildKit cannot read that. The spike pushed the base into the bundled registry and built `FROM <front>/last/images@<digest>`.
8. **`kind load docker-image` failed for multi-arch images** under Docker 29's containerd image store: `ctr: content digest …: not found`. The pods pulled from Docker Hub instead. Single-platform images such as ours load fine.

## Not covered

- **EKS and GKE Standard.** They need cloud accounts. M4 should ship a script that deploys the stack and runs this pull test against any kube context, so the check can be run on those platforms (see the M4 design).
- **Cilium hostPort** (`hostPort.enabled` with kube-proxy replacement), and nodes whose `localhost` resolves to `::1` first.
- **Builds on k3s.**
