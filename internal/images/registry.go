// Package images is the client for the OCI registries that hold built images: the
// bundled one behind its front proxy, or an external one.
package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Credentials authenticate to the front proxy (spec §11.3).
type Credentials struct{ Username, Password string }

// Client talks to one registry host such as "localhost:15000" or
// "last-registry-front.last-registry.svc:5000".
type Client struct {
	host     string
	creds    *Credentials
	insecure bool
}

// NewClient returns a client for host. insecure selects plain HTTP. A nil creds
// means anonymous access.
func NewClient(host string, creds *Credentials, insecure bool) *Client {
	return &Client{host: host, creds: creds, insecure: insecure}
}

func (c *Client) nameOpts() []name.Option {
	if c.insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

func (c *Client) remoteOpts(ctx context.Context) []remote.Option {
	auth := authn.Anonymous
	if c.creds != nil {
		auth = &authn.Basic{Username: c.creds.Username, Password: c.creds.Password}
	}
	return []remote.Option{remote.WithAuth(auth), remote.WithContext(ctx)}
}

func (c *Client) digestRef(repo, digest string) (name.Digest, error) {
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", c.host, repo, digest), c.nameOpts()...)
	if err != nil {
		return name.Digest{}, fmt.Errorf("parse image reference: %w", err)
	}
	return ref, nil
}

// isNotFound reports whether err is a registry 404.
func isNotFound(err error) bool {
	var terr *transport.Error
	return errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound
}

// Head reports whether repo has a manifest with this digest ("sha256:<64 hex>").
// 404 is (false, nil); any other failure is an error.
func (c *Client) Head(ctx context.Context, repo, digest string) (bool, error) {
	ref, err := c.digestRef(repo, digest)
	if err != nil {
		return false, err
	}
	if _, err := remote.Head(ref, c.remoteOpts(ctx)...); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("head %s: %w", ref, err)
	}
	return true, nil
}

// Delete deletes the manifest; deleting one that does not exist is not an error.
func (c *Client) Delete(ctx context.Context, repo, digest string) error {
	ref, err := c.digestRef(repo, digest)
	if err != nil {
		return err
	}
	if err := remote.Delete(ref, c.remoteOpts(ctx)...); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete %s: %w", ref, err)
	}
	return nil
}

// Push writes img as repo:tag and returns its manifest digest.
func (c *Client) Push(ctx context.Context, repo, tag string, img v1.Image) (string, error) {
	ref, err := name.NewTag(fmt.Sprintf("%s/%s:%s", c.host, repo, tag), c.nameOpts()...)
	if err != nil {
		return "", fmt.Errorf("parse image reference: %w", err)
	}
	if err := remote.Write(ref, img, c.remoteOpts(ctx)...); err != nil {
		return "", fmt.Errorf("push %s: %w", ref, err)
	}
	d, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("compute digest: %w", err)
	}
	return d.String(), nil
}
