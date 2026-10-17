package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

const (
	pusherPW = "s3cret-pw"
	pusher   = "pusher"
	tarFlag  = "--tar"
	toFlag   = "--to"
)

// frontProxy mimics the nginx front proxy's rules: GET and HEAD are anonymous
// and every other method needs the pusher credential.
func frontProxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			user, pw, ok := r.BasicAuth()
			if !ok || user != pusher || pw != pusherPW {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func newRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(frontProxy(registry.New()))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func writeTar(t *testing.T) string {
	t.Helper()
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewTag("example.com/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "img.tar")
	if err := tarball.WriteToFile(file, ref, img); err != nil {
		t.Fatal(err)
	}
	return file
}

func pusherEnv(k string) string {
	switch k {
	case "LAST_REGISTRY_USERNAME":
		return pusher
	case "LAST_REGISTRY_PASSWORD":
		return pusherPW
	}
	return ""
}

func TestPushesATarballAndPrintsTheDigest(t *testing.T) {
	host, file := newRegistry(t), writeTar(t)
	var out, errb bytes.Buffer
	code := run([]string{toFlag, host + "/team/app:v1", tarFlag, file}, pusherEnv, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errb.String())
	}
	got := out.String()
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64+1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("stdout = %q, want a digest and one newline", got)
	}
}

func TestALabelChangesTheDigest(t *testing.T) {
	host, file := newRegistry(t), writeTar(t)
	push := func(extra ...string) string {
		var out, errb bytes.Buffer
		args := append([]string{toFlag, host + "/team/app:v1", tarFlag, file}, extra...)
		if code := run(args, pusherEnv, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, stderr: %s", code, errb.String())
		}
		return out.String()
	}
	plain, labelled := push(), push("--label", "last.check=abc")
	if plain == labelled {
		t.Fatalf("label did not change the digest: %s", plain)
	}
}

func TestRefusesBothOrNeitherSource(t *testing.T) {
	for _, args := range [][]string{
		{toFlag, "localhost:1/a:v1", tarFlag, "x.tar", "--from", "busybox"},
		{toFlag, "localhost:1/a:v1"},
	} {
		var out, errb bytes.Buffer
		if code := run(args, pusherEnv, &out, &errb); code != 1 {
			t.Fatalf("%v: exit = %d, want 1", args, code)
		}
		if !strings.Contains(errb.String(), "Usage") || out.Len() != 0 {
			t.Fatalf("%v: stderr = %q, stdout = %q", args, errb.String(), out.String())
		}
	}
}

func TestNeverPrintsThePassword(t *testing.T) {
	host, file := newRegistry(t), writeTar(t)
	var out, errb bytes.Buffer
	// Anonymous: the proxy refuses the write. The password is only in the env of
	// a different run, so also run once with a wrong password that must not echo.
	anon := func(string) string { return "" }
	if code := run([]string{toFlag, host + "/team/app:v1", tarFlag, file}, anon, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	wrong := func(k string) string {
		if k == "LAST_REGISTRY_PASSWORD" {
			return "wrong-" + pusherPW
		}
		return pusher
	}
	if code := run([]string{toFlag, host + "/team/app:v1", tarFlag, file}, wrong, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, s := range []string{out.String(), errb.String()} {
		if strings.Contains(s, pusherPW) {
			t.Fatalf("output leaks the password: %q", s)
		}
	}
	if errb.Len() == 0 {
		t.Fatal("expected an error on stderr")
	}
}
