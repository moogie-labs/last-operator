// Package audittest provides an in-memory audit.Sink with tamper helpers for tests.
package audittest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
)

type version struct {
	id   string
	body []byte
}

type object struct {
	versions      []version
	deleteMarkers int
}

// MemSink is an in-memory audit.Sink that honors create-only semantics.
type MemSink struct {
	mu       sync.Mutex
	objects  map[string]*object
	lock     audit.LockConfig
	failPuts bool
	nextID   int
}

func NewMemSink() *MemSink {
	return &MemSink{
		objects: map[string]*object{},
		lock:    audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: 365 * 24 * time.Hour},
	}
}

func (m *MemSink) SetLockConfig(lc audit.LockConfig) { m.mu.Lock(); defer m.mu.Unlock(); m.lock = lc }
func (m *MemSink) SetFailPuts(fail bool)             { m.mu.Lock(); defer m.mu.Unlock(); m.failPuts = fail }

func (m *MemSink) Put(_ context.Context, key string, body []byte, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPuts {
		return errors.New("memsink: store unavailable")
	}
	if o, ok := m.objects[key]; ok && len(o.versions) > 0 {
		return audit.ErrExists
	}
	m.addVersionLocked(key, body)
	return nil
}

func (m *MemSink) addVersionLocked(key string, body []byte) {
	o := m.objects[key]
	if o == nil {
		o = &object{}
		m.objects[key] = o
	}
	m.nextID++
	o.versions = append(o.versions, version{id: fmt.Sprintf("v%d", m.nextID), body: bytes.Clone(body)})
}

func (m *MemSink) Get(_ context.Context, key, versionID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok || len(o.versions) == 0 {
		return nil, audit.ErrNotFound
	}
	if versionID == "" {
		return bytes.Clone(o.versions[0].body), nil
	}
	for _, v := range o.versions {
		if v.id == versionID {
			return bytes.Clone(v.body), nil
		}
	}
	return nil, audit.ErrNotFound
}

func (m *MemSink) List(_ context.Context, prefix, startAfter string) ([]audit.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []audit.ObjectInfo
	for k, o := range m.objects {
		if !strings.HasPrefix(k, prefix) || k <= startAfter {
			continue
		}
		info := audit.ObjectInfo{Key: k, DeleteMarkers: o.deleteMarkers}
		for _, v := range o.versions {
			info.VersionIDs = append(info.VersionIDs, v.id)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *MemSink) LockConfig(context.Context) (audit.LockConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lock, nil
}

// Keys returns every stored key, sorted.
func (m *MemSink) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The Tamper* helpers bypass Object Lock to simulate a compromised or non-conformant store.

func (m *MemSink) TamperDelete(key string) { m.mu.Lock(); defer m.mu.Unlock(); delete(m.objects, key) }

func (m *MemSink) TamperReplace(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objects[key]; ok && len(o.versions) > 0 {
		o.versions[0].body = bytes.Clone(body)
	}
}

func (m *MemSink) TamperAddVersion(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addVersionLocked(key, body)
}

func (m *MemSink) TamperAddDeleteMarker(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objects[key]; ok {
		o.deleteMarkers++
	}
}

// TamperPut writes key as a brand-new object even if it already exists.
func (m *MemSink) TamperPut(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	m.addVersionLocked(key, body)
}
