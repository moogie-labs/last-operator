package audit

import (
	"context"
	"errors"
	"time"
)

var (
	ErrExists   = errors.New("audit: object already exists")
	ErrNotFound = errors.New("audit: object not found")
)

// ObjectInfo describes every stored version of one key.
type ObjectInfo struct {
	Key           string
	VersionIDs    []string // oldest first
	DeleteMarkers int
}

// LockConfig is a bucket's default Object Lock configuration.
type LockConfig struct {
	Enabled          bool
	Mode             string // "COMPLIANCE", "GOVERNANCE", or ""
	DefaultRetention time.Duration
}

// Sink is WORM object storage for audit entries.
type Sink interface {
	// Put writes body at key only if key does not exist (If-None-Match: *), locked in
	// compliance mode until retainUntil. It returns ErrExists if key already exists.
	Put(ctx context.Context, key string, body []byte, retainUntil time.Time) error
	// Get reads one version of key; versionID "" means the oldest (original) version.
	Get(ctx context.Context, key, versionID string) ([]byte, error)
	// List returns objects under prefix whose keys sort after startAfter, sorted by key.
	List(ctx context.Context, prefix, startAfter string) ([]ObjectInfo, error)
	// LockConfig reports the bucket's default Object Lock configuration.
	LockConfig(ctx context.Context) (LockConfig, error)
}
