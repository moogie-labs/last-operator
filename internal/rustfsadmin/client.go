// Package rustfsadmin is a minimal client for RustFS's native admin API
// (/rustfs/admin/v3/*), which takes plain JSON bodies signed with SigV4.
package rustfsadmin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// APIError is a non-2xx admin API response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("rustfs admin: HTTP %d: %s", e.Status, e.Body) }

type Client struct {
	endpoint string
	creds    aws.Credentials
	signer   *v4.Signer
	http     *http.Client
}

func New(endpoint, accessKey, secretKey string) *Client {
	return &Client{
		endpoint: strings.TrimSuffix(endpoint, "/"),
		creds:    aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		signer:   v4.NewSigner(),
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// AddCannedPolicy creates or replaces a named IAM policy.
func (c *Client) AddCannedPolicy(ctx context.Context, name string, policy []byte) error {
	return c.do(ctx, "add-canned-policy", url.Values{"name": {name}}, policy)
}

// AddUser creates or updates an enabled user.
func (c *Client) AddUser(ctx context.Context, accessKey, secretKey string) error {
	body, err := json.Marshal(map[string]string{"secretKey": secretKey, "status": "enabled"})
	if err != nil {
		return err
	}
	return c.do(ctx, "add-user", url.Values{"accessKey": {accessKey}}, body)
}

// SetUserPolicy attaches a named policy to a user.
func (c *Client) SetUserPolicy(ctx context.Context, policyName, user string) error {
	return c.do(ctx, "set-user-or-group-policy",
		url.Values{"policyName": {policyName}, "userOrGroup": {user}, "isGroup": {"false"}}, nil)
}

func (c *Client) do(ctx context.Context, op string, query url.Values, body []byte) error {
	u := c.endpoint + "/rustfs/admin/v3/" + op + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := c.signer.SignHTTP(ctx, c.creds, req, payloadHash, "s3", "us-east-1", time.Now()); err != nil {
		return fmt.Errorf("rustfs admin: sign: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("rustfs admin: %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	return nil
}
