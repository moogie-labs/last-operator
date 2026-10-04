#!/usr/bin/env bash
# Run the sandbox image the way the gateway will: read-only root, tmpfs /work and /tmp,
# spec mounted at /last/spec. Prints the envelope and checks it.
set -euo pipefail
IMAGE="${1:?usage: sandbox-smoke.sh <image>}"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
cat > "$dir/spec.json" <<'JSON'
{"version":1,"commands":[
 {"id":"tools","script":"id -u; kubectl version --client >/dev/null && jq --version >/dev/null && git --version >/dev/null && python3 --version >/dev/null && curl --version >/dev/null && echo tools-ok","timeoutSeconds":30},
 {"id":"files","files":{"/work/hello.txt":"hi"},"argv":["cat","/work/hello.txt"],"timeoutSeconds":30},
 {"id":"rootfs","script":"touch /etc/x 2>&1 | grep -q 'Read-only file system' && echo readonly || echo not-readonly","timeoutSeconds":30},
 {"id":"home","script":"whoami && touch /tmp/a /work/b && git config --global user.name sandbox && echo home-ok","timeoutSeconds":30}
]}
JSON
out="$(docker run --rm --read-only --tmpfs /work:uid=65532,gid=65532 --tmpfs /tmp \
  -v "$dir:/last/spec:ro" --cap-drop ALL --security-opt no-new-privileges "$IMAGE")"
echo "$out"
line="$(printf '%s\n' "$out" | grep '^LAST-RESULT ' | sed 's/^LAST-RESULT //')"
echo "$line" | jq -e '.results[0].stdout == "65532\ntools-ok\n"' >/dev/null
echo "$line" | jq -e '.results[1].stdout == "hi"' >/dev/null
# EROFS (not EACCES) proves the root filesystem itself is read-only.
echo "$line" | jq -e '.results[2].stdout == "readonly\n"' >/dev/null
# A passwd entry for 65532 and a writable HOME/TMPDIR, as git, ssh and python expect.
echo "$line" | jq -e '.results[3].stdout == "sandbox\nhome-ok\n"' >/dev/null
echo "sandbox smoke test passed"
