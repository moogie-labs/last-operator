//go:build integration

// Package rustfstest starts a disposable RustFS server for integration tests.
package rustfstest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/moogie-labs/last-operator/internal/audit"
)

const (
	Image         = "rustfs/rustfs:1.0.0"
	RootAccessKey = "last-root"
	RootSecretKey = "last-root-secret-key-for-tests"
)

type Server struct {
	Endpoint  string
	container testcontainers.Container
}

// Start runs RustFS and waits until /health/ready answers. It is terminated at test cleanup.
func Start(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{"9000/tcp"},
			Env: map[string]string{
				"RUSTFS_ACCESS_KEY":     RootAccessKey,
				"RUSTFS_SECRET_KEY":     RootSecretKey,
				"RUSTFS_VOLUMES":        "/data",
				"RUSTFS_ADDRESS":        ":9000",
				"RUSTFS_CONSOLE_ENABLE": "false",
			},
			WaitingFor: wait.ForHTTP("/health/ready").WithPort("9000/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start rustfs: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Endpoint: fmt.Sprintf("http://%s:%s", host, port.Port()), container: c}
}

// Client returns an S3 client for the given credentials.
func (s *Server) Client(t *testing.T, accessKey, secretKey string) *s3.Client {
	t.Helper()
	c, err := audit.NewS3Client(context.Background(), audit.S3Config{
		Endpoint: s.Endpoint, AccessKey: accessKey, SecretKey: secretKey, PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (s *Server) RootClient(t *testing.T) *s3.Client {
	return s.Client(t, RootAccessKey, RootSecretKey)
}

// Stop halts the container without removing it, simulating an outage.
func (s *Server) Stop(t *testing.T) {
	t.Helper()
	timeout := 10 * time.Second
	if err := s.container.Stop(context.Background(), &timeout); err != nil {
		t.Fatal(err)
	}
}
