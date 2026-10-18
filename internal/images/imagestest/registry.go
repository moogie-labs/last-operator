// Package imagestest serves an in-memory OCI registry over TLS with basic auth, as
// an external registry (ECR, GHCR, Harbor) would, for tests.
package imagestest

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/moogie-labs/last-operator/internal/images"
)

// certHost is a name the httptest certificate holds. A loopback address would not
// do: clients fall back to plain HTTP for those, so HTTPS would go untested.
const certHost = "example.com"

// refusal is how the registry answers a DELETE of one digest.
type refusal struct {
	status int
	body   string
}

// Registry is a TLS registry that admits only its one credential.
type Registry struct {
	// Host is "example.com:<port>"; only Transport reaches it.
	Host string
	// Transport trusts the server's certificate and dials it for Host.
	Transport      http.RoundTripper
	User, Password string

	// Heads and Deletes count authenticated manifest HEADs and DELETEs.
	Heads, Deletes atomic.Int32

	mu       sync.Mutex
	refusals map[string]refusal
}

// New starts a registry whose only credential is user:password.
func New(t testing.TB, user, password string) *Registry {
	t.Helper()
	r := &Registry{User: user, Password: password, refusals: map[string]refusal{}}
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if u, p, ok := req.BasicAuth(); !ok || u != user || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="imagestest"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch req.Method {
		case http.MethodHead:
			if strings.Contains(req.URL.Path, "/manifests/") {
				r.Heads.Add(1)
			}
		case http.MethodDelete:
			r.Deletes.Add(1)
			r.mu.Lock()
			ref, refused := r.refusals[req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]]
			r.mu.Unlock()
			if refused {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(ref.status)
				_, _ = io.WriteString(w, ref.body)
				return
			}
		}
		reg.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	r.Host = certHost + ":" + port
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	r.Transport = tr
	return r
}

// RefuseDelete makes a DELETE of digest answer status with body.
func (r *Registry) RefuseDelete(digest string, status int, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusals[digest] = refusal{status: status, body: body}
}

// Client is an authenticated HTTPS client for the registry.
func (r *Registry) Client() *images.Client {
	return images.NewClient(r.Host, &images.Credentials{Username: r.User, Password: r.Password}, false).WithTransport(r.Transport)
}

// Push puts a random image into repo (a path on Host) and returns its digest.
func (r *Registry) Push(t testing.TB, repo string) string {
	t.Helper()
	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.Client().Push(context.Background(), repo, "seed", img)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Has reports whether repo holds a manifest with digest.
func (r *Registry) Has(t testing.TB, repo, digest string) bool {
	t.Helper()
	found, err := r.Client().Head(context.Background(), repo, digest)
	if err != nil {
		t.Fatal(err)
	}
	return found
}
