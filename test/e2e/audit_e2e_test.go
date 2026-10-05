//go:build e2e

package e2e

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	kubectlLast string
	tmpDir      string
)

func TestMain(m *testing.M) {
	ctrllog.SetLogger(logr.Discard())
	dir, err := os.MkdirTemp("", "last-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tmpDir = dir
	kubectlLast = filepath.Join(dir, "kubectl-last")
	build := exec.Command("go", "build", "-o", kubectlLast, "../../cmd/kubectl-last")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build kubectl-last:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("kubectl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func secretValue(t *testing.T, ns, name, key string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(kubectl(t, "get", "secret", "-n", ns, name, "-o", "jsonpath={.data."+key+"}"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// portForward forwards a random local port to target in namespace and returns 127.0.0.1:<port>.
func portForward(t *testing.T, namespace, target, remotePort string) string {
	t.Helper()
	cmd := exec.Command("kubectl", "port-forward", "-n", namespace, target, ":"+remotePort)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	re := regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() { // keep draining so port-forward never blocks on stdout
			if m := re.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case port := <-found:
		return "127.0.0.1:" + port
	case <-time.After(30 * time.Second):
		t.Fatal("kubectl port-forward did not start")
		return ""
	}
}

type env map[string]string

func runLast(e env, args ...string) (string, error) {
	cmd := exec.Command(kubectlLast, args...)
	cmd.Env = os.Environ()
	for k, v := range e {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var headRe = regexp.MustCompile(`seq \d+\.\.(\d+)`)

func headSeq(t *testing.T, verifyOutput string) uint64 {
	t.Helper()
	m := headRe.FindStringSubmatch(verifyOutput)
	if m == nil {
		t.Fatalf("no head seq in: %s", verifyOutput)
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	return n
}

func TestAuditChain(t *testing.T) {
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	chain := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-config", "-o", "jsonpath={.data.chainId}")
	pub := filepath.Join(tmpDir, "pub.pem")
	pubPEM := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-pubkey", "-o", `jsonpath={.data.ed25519\.pub}`)
	if err := os.WriteFile(pub, []byte(pubPEM+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := func(endpoint, ns, secret string) env {
		return env{
			"LAST_AUDIT_ENDPOINT": endpoint, "LAST_AUDIT_BUCKET": "last-audit", "LAST_AUDIT_CHAIN_ID": chain,
			"AWS_ACCESS_KEY_ID":     secretValue(t, ns, secret, "accessKey"),
			"AWS_SECRET_ACCESS_KEY": secretValue(t, ns, secret, "secretKey"),
		}
	}
	endpoint := "http://" + portForward(t, "last-audit", "svc/last-rustfs", "9000")
	reader := creds(endpoint, "last-audit", "last-audit-reader")

	var before uint64
	t.Run("chain verifies after install", func(t *testing.T) {
		out, err := runLast(reader, "audit", "verify", "--pubkey-file", pub)
		if err != nil {
			t.Fatalf("verify: %v\n%s", err, out)
		}
		before = headSeq(t, out)
	})

	t.Run("gateway restart extends the chain", func(t *testing.T) {
		kubectl(t, "rollout", "restart", "-n", "last-gateway", "deploy/last-gateway")
		kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
		out, err := runLast(reader, "audit", "verify", "--pubkey-file", pub)
		if err != nil {
			t.Fatalf("verify: %v\n%s", err, out)
		}
		if after := headSeq(t, out); after <= before {
			t.Fatalf("head did not advance after restart: before=%d after=%d", before, after)
		}
	})

	t.Run("bundled store passes WORM conformance", func(t *testing.T) {
		e := creds(endpoint, "last-gateway", "last-audit-writer")
		e["LAST_ADMIN_ACCESS_KEY"] = secretValue(t, "last-audit", "last-audit-root", "RUSTFS_ACCESS_KEY")
		e["LAST_ADMIN_SECRET_KEY"] = secretValue(t, "last-audit", "last-audit-root", "RUSTFS_SECRET_KEY")
		out, err := runLast(e, "audit", "conformance")
		if err != nil || strings.Contains(out, "FAIL") {
			t.Fatalf("conformance: %v\n%s", err, out)
		}
	})

	t.Run("gateway refuses to start while the store is down", func(t *testing.T) {
		kubectl(t, "scale", "-n", "last-audit", "statefulset/last-rustfs", "--replicas=0")
		kubectl(t, "wait", "-n", "last-audit", "--for=delete", "pod/last-rustfs-0", "--timeout=2m")
		kubectl(t, "rollout", "restart", "-n", "last-gateway", "deploy/last-gateway")
		time.Sleep(45 * time.Second)
		ready := kubectl(t, "get", "deploy", "-n", "last-gateway", "last-gateway", "-o", "jsonpath={.status.readyReplicas}")
		if ready != "" && ready != "0" {
			t.Fatalf("gateway reported ready (%s) with the audit store down", ready)
		}

		kubectl(t, "scale", "-n", "last-audit", "statefulset/last-rustfs", "--replicas=1")
		kubectl(t, "rollout", "status", "-n", "last-audit", "statefulset/last-rustfs", "--timeout=5m")
		kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=10m")
		fresh := creds("http://"+portForward(t, "last-audit", "svc/last-rustfs", "9000"), "last-audit", "last-audit-reader") // the old forward died with the pod
		out, err := runLast(fresh, "audit", "verify", "--pubkey-file", pub)
		if err != nil {
			t.Fatalf("verify after recovery: %v\n%s", err, out)
		}
	})
}
