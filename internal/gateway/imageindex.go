package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	imageIndexName      = "last-image-index"
	imageIndexKey       = "index.json"
	imageIndexComponent = "image-index"
	// imageFlushEvery bounds how often an entry's lastUsed is written back.
	imageFlushEvery = time.Hour

	buildSucceeded = "succeeded"
)

// imageEntry is one built image: its digest and the signed image.build.result
// entry that names it.
type imageEntry struct {
	Digest   string    `json:"digest"`
	BuildSeq uint64    `json:"buildSeq"`
	LastUsed time.Time `json:"lastUsed"`
}

// buildResult is the image.build.result audit payload. A digest in the index is
// trusted only when a signed entry of this shape names it.
type buildResult struct {
	SpecHash     string `json:"specHash"`
	StartSeq     uint64 `json:"startSeq"`
	Outcome      string `json:"outcome"` // "succeeded" | "failed"
	Digest       string `json:"digest,omitempty"`
	DurationMs   int64  `json:"durationMs"`
	Log          string `json:"log"`
	LogTruncated bool   `json:"logTruncated"`
	Error        string `json:"error,omitempty"`
}

// imageIndex caches spec hash -> built image in a ConfigMap. It is only a cache:
// verifyBuild checks every entry against the signed audit log before it is used.
type imageIndex struct {
	kube      kubernetes.Interface
	namespace string

	mu       sync.Mutex
	cm       *corev1.ConfigMap // as last loaded or saved; nil when none exists yet
	entries  map[string]imageEntry
	verified map[string]bool      // verifyBuild passes, per process
	flushed  map[string]time.Time // the lastUsed last written for each entry
}

func newImageIndex(kube kubernetes.Interface, namespace string) *imageIndex {
	return &imageIndex{kube: kube, namespace: namespace, entries: map[string]imageEntry{},
		verified: map[string]bool{}, flushed: map[string]time.Time{}}
}

// load reads the ConfigMap; a missing one is an empty index.
func (x *imageIndex) load(ctx context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.loadLocked(ctx)
}

func (x *imageIndex) loadLocked(ctx context.Context) error {
	cm, err := x.kube.CoreV1().ConfigMaps(x.namespace).Get(ctx, imageIndexName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		x.cm, x.entries, x.flushed = nil, map[string]imageEntry{}, map[string]time.Time{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("gateway: load image index: %w", err)
	}
	entries := map[string]imageEntry{}
	if raw := cm.Data[imageIndexKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			// The index is only a cache: treat it as lost, keep the ConfigMap for its
			// resourceVersion, and let the next save overwrite it.
			slog.Error("Image index ConfigMap is corrupt; treating it as empty", "name", cm.Name, "err", err)
			entries = map[string]imageEntry{}
		}
	}
	x.cm, x.entries, x.flushed = cm, entries, make(map[string]time.Time, len(entries))
	for k, e := range entries {
		x.flushed[k] = e.LastUsed
	}
	return nil
}

func (x *imageIndex) isVerified(key string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.verified[key]
}

func (x *imageIndex) markVerified(key string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.verified[key] = true
}

func (x *imageIndex) get(specHash string) (imageEntry, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.entries[specHash]
	return e, ok
}

// put saves the entry at once.
func (x *imageIndex) put(ctx context.Context, specHash string, e imageEntry) error {
	return x.mutate(ctx, func() { x.entries[specHash] = e })
}

// drop removes the entry, and any verification cached for it, and saves.
func (x *imageIndex) drop(ctx context.Context, specHash string) error {
	return x.mutate(ctx, func() {
		delete(x.entries, specHash)
		delete(x.flushed, specHash)
		for k := range x.verified {
			if strings.HasPrefix(k, specHash+"|") {
				delete(x.verified, k)
			}
		}
	})
}

// touch records a use in memory only; flushUsed writes it back.
func (x *imageIndex) touch(specHash string, now time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if e, ok := x.entries[specHash]; ok && now.After(e.LastUsed) {
		e.LastUsed = now
		x.entries[specHash] = e
	}
}

// flushUsed saves the index when some entry's lastUsed changed and that entry was
// last written over an hour ago.
func (x *imageIndex) flushUsed(ctx context.Context, now time.Time) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	due := false
	for k, e := range x.entries {
		if e.LastUsed.After(x.flushed[k]) && now.Sub(x.flushed[k]) >= imageFlushEvery {
			due = true
			break
		}
	}
	if !due {
		return nil
	}
	return x.saveRetryLocked(ctx, func() {})
}

// mutate applies change and saves.
func (x *imageIndex) mutate(ctx context.Context, change func()) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.saveRetryLocked(ctx, change)
}

// saveRetryLocked applies change and saves. On a conflict (another writer, or a
// create race) it reloads, applies change again, and saves once more.
func (x *imageIndex) saveRetryLocked(ctx context.Context, change func()) error {
	change()
	err := x.saveLocked(ctx)
	if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
		return err
	}
	local := x.entries
	if err := x.loadLocked(ctx); err != nil {
		return err
	}
	// Keep the in-memory uses the reload would lose.
	for k, e := range x.entries {
		if l, ok := local[k]; ok && l.Digest == e.Digest && l.LastUsed.After(e.LastUsed) {
			e.LastUsed = l.LastUsed
			x.entries[k] = e
		}
	}
	change()
	return x.saveLocked(ctx)
}

func (x *imageIndex) saveLocked(ctx context.Context) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(x.entries); err != nil {
		return fmt.Errorf("gateway: encode image index: %w", err)
	}
	body := strings.TrimSuffix(buf.String(), "\n")
	cms := x.kube.CoreV1().ConfigMaps(x.namespace)
	var saved *corev1.ConfigMap
	var err error
	if x.cm == nil {
		saved, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: imageIndexName, Namespace: x.namespace,
				Labels: map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, labelComponent: imageIndexComponent}},
			Data: map[string]string{imageIndexKey: body},
		}, metav1.CreateOptions{})
	} else {
		cm := x.cm.DeepCopy()
		cm.Data = map[string]string{imageIndexKey: body}
		saved, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("gateway: save image index: %w", err)
	}
	x.cm = saved
	for k, e := range x.entries {
		x.flushed[k] = e.LastUsed
	}
	return nil
}

// verifyBuild reads the signed entry at e.BuildSeq and checks it is the
// image.build.result that produced e.Digest for specHash with outcome
// "succeeded". Anything else means the index was edited or is stale; the caller
// drops the entry and rebuilds. A pass is cached for the life of the process.
func (g *Gateway) verifyBuild(ctx context.Context, specHash string, e imageEntry) error {
	key := specHash + "|" + e.Digest + "|" + strconv.FormatUint(e.BuildSeq, 10)
	if g.images.isVerified(key) {
		return nil
	}
	if e.Digest == "" {
		return errors.New("image index entry has no digest")
	}
	body, err := g.sink.Get(ctx, audit.EntryKey(g.cfg.ChainID, e.BuildSeq), "")
	if errors.Is(err, audit.ErrNotFound) {
		return fmt.Errorf("audit entry %d does not exist", e.BuildSeq)
	}
	if err != nil {
		return fmt.Errorf("read audit entry %d: %w", e.BuildSeq, err)
	}
	entry, err := audit.DecodeEntry(body)
	if err != nil {
		return fmt.Errorf("audit entry %d failed verification: %w", e.BuildSeq, err)
	}
	if err := audit.CheckSeal(g.signer.Public(), entry); err != nil {
		return fmt.Errorf("audit entry %d failed verification: %w", e.BuildSeq, err)
	}
	if entry.Seq != e.BuildSeq || entry.Type != audit.TypeImageBuildResult {
		return fmt.Errorf("audit entry %d is not an %s", e.BuildSeq, audit.TypeImageBuildResult)
	}
	var r buildResult
	if err := json.Unmarshal(entry.Payload, &r); err != nil {
		return fmt.Errorf("audit entry %d has an unreadable payload: %w", e.BuildSeq, err)
	}
	if r.Outcome != buildSucceeded || r.SpecHash != specHash || r.Digest != e.Digest {
		return fmt.Errorf("audit entry %d does not record a successful build of %s as %s", e.BuildSeq, specHash, e.Digest)
	}
	g.images.markVerified(key)
	return nil
}
