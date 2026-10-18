package images_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/moogie-labs/last-operator/internal/images"
	"github.com/moogie-labs/last-operator/internal/images/imagestest"
)

const (
	pusherPW = "s3cret-pw"
	adminPW  = "adm1n-pw"
)

var (
	pusher = &images.Credentials{Username: testPusher, Password: pusherPW}
	admin  = &images.Credentials{Username: "admin", Password: adminPW}
)

// frontProxy mimics the nginx front proxy's rules: GET and HEAD are anonymous,
// DELETE needs the admin credential and every other method needs the pusher one.
func frontProxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodDelete:
			if !ok || user != "admin" || pw != adminPW {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		default:
			if !ok || user != "pusher" || pw != pusherPW {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// newRegistry serves an in-memory registry behind frontProxy and returns its host.
func newRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(frontProxy(registry.New()))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestPushThenHeadFindsTheManifest(t *testing.T) {
	host := newRegistry(t)
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	want, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := images.NewClient(host, pusher, true).Push(ctx, "team/app", "v1", img)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got != want.String() {
		t.Fatalf("Push digest = %s, want %s", got, want)
	}
	found, err := images.NewClient(host, nil, true).Head(ctx, "team/app", got)
	if err != nil || !found {
		t.Fatalf("Head = %v, %v; want true, nil", found, err)
	}
}

func TestHeadOfAMissingDigestIsFalse(t *testing.T) {
	host := newRegistry(t)
	found, err := images.NewClient(host, nil, true).Head(context.Background(), "team/app", "sha256:"+strings.Repeat("0", 64))
	if err != nil || found {
		t.Fatalf("Head = %v, %v; want false, nil", found, err)
	}
}

func TestDeleteRemovesTheManifest(t *testing.T) {
	host := newRegistry(t)
	img, _ := random.Image(1024, 1)
	ctx := context.Background()
	digest, err := images.NewClient(host, pusher, true).Push(ctx, "team/app", "v1", img)
	if err != nil {
		t.Fatal(err)
	}
	c := images.NewClient(host, admin, true)
	if err := c.Delete(ctx, "team/app", digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if found, err := c.Head(ctx, "team/app", digest); err != nil || found {
		t.Fatalf("Head after Delete = %v, %v; want false, nil", found, err)
	}
	if err := c.Delete(ctx, "team/app", digest); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestPusherCannotDelete(t *testing.T) {
	host := newRegistry(t)
	img, _ := random.Image(1024, 1)
	ctx := context.Background()
	c := images.NewClient(host, pusher, true)
	digest, err := c.Push(ctx, "team/app", "v1", img)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "team/app", digest); err == nil {
		t.Fatal("Delete as pusher succeeded, want an error")
	}
}

func TestHeadReportsAuthFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	found, err := images.NewClient(host, pusher, true).Head(context.Background(), "team/app", "sha256:"+strings.Repeat("0", 64))
	if err == nil || found {
		t.Fatalf("Head = %v, %v; want false and an error", found, err)
	}
	if strings.Contains(err.Error(), pusherPW) {
		t.Fatalf("error leaks the password: %v", err)
	}
}

// A client that is not insecure never falls back to plain HTTP, even for a loopback
// or private host where go-containerregistry would, so its credential never travels
// in the clear.
func TestAnHTTPSClientNeverFallsBackToHTTP(t *testing.T) {
	var sawAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		registry.New().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	digest := "sha256:" + strings.Repeat("0", 64)
	found, err := images.NewClient(host, pusher, false).Head(context.Background(), "team/app", digest)
	if err == nil || found {
		t.Fatalf("Head over plain HTTP = %v, %v; want an error", found, err)
	}
	if err := images.NewClient(host, pusher, false).Delete(context.Background(), "team/app", digest); err == nil {
		t.Fatal("Delete over plain HTTP succeeded")
	}
	if sawAuth.Load() {
		t.Fatal("the credential was sent over plain HTTP")
	}
}

func TestDeleteReportsARegistryThatRefusesDelete(t *testing.T) {
	for name, refuse := range map[string]func(w http.ResponseWriter){
		"405": func(w http.ResponseWriter) { w.WriteHeader(http.StatusMethodNotAllowed) },
		"UNSUPPORTED": func(w http.ResponseWriter) {
			http.Error(w, `{"errors":[{"code":"UNSUPPORTED"}]}`, http.StatusBadRequest)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					refuse(w)
					return
				}
				registry.New().ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			err := images.NewClient(strings.TrimPrefix(srv.URL, "http://"), admin, true).
				Delete(context.Background(), "team/app", "sha256:"+strings.Repeat("0", 64))
			if !errors.Is(err, images.ErrDeleteUnsupported) {
				t.Fatalf("Delete = %v, want ErrDeleteUnsupported", err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	err := images.NewClient(strings.TrimPrefix(srv.URL, "http://"), admin, true).
		Delete(context.Background(), "team/app", "sha256:"+strings.Repeat("0", 64))
	if err == nil || errors.Is(err, images.ErrDeleteUnsupported) {
		t.Fatalf("Delete answered 500 = %v, want an ordinary error", err)
	}
}

// A registry at a loopback or private address is still used over HTTPS:
// go-containerregistry addresses it over plain HTTP, and the client upgrades those
// requests to its own host.
func TestAnHTTPSClientReachesARegistryAtAPrivateAddress(t *testing.T) {
	ext := imagestest.New(t, "robot", "ext-s3cret")
	creds := &images.Credentials{Username: ext.User, Password: ext.Password}
	c := images.NewClient(ext.Loopback, creds, false).WithTransport(ext.Transport)
	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	digest, err := c.Push(ctx, "acme/app", "v1", img)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if found, err := c.Head(ctx, "acme/app", digest); err != nil || !found {
		t.Fatalf("Head = %v, %v; want true", found, err)
	}
	if err := c.Delete(ctx, "acme/app", digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if found, err := c.Head(ctx, "acme/app", digest); err != nil || found {
		t.Fatalf("Head after Delete = %v, %v; want false", found, err)
	}
}

// Only the client's own registry host is upgraded: a token realm on another host
// over plain HTTP is refused, so the credential never reaches it.
func TestAnHTTPSClientRefusesAPlainHTTPTokenRealm(t *testing.T) {
	var realmHits atomic.Int32
	realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		realmHits.Add(1)
		_, _ = io.WriteString(w, `{"token":"t"}`)
	}))
	t.Cleanup(realm.Close)
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm.URL+`/token",service="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(reg.Close)
	c := images.NewClient(strings.TrimPrefix(reg.URL, "https://"), pusher, false).WithTransport(reg.Client().Transport)
	if found, err := c.Head(context.Background(), "acme/app", "sha256:"+strings.Repeat("0", 64)); err == nil || found {
		t.Fatalf("Head = %v, %v; want an error", found, err)
	}
	if realmHits.Load() != 0 {
		t.Fatalf("the plain-HTTP token realm got %d requests", realmHits.Load())
	}
}
