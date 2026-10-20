package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
)

// indexedTypes are the audit entry types QueryAudit serves (M3 spec §3.5).
var indexedTypes = map[string]bool{
	audit.TypeLLMRequest: true, audit.TypeLLMResponse: true,
	audit.TypeCommandStart: true, audit.TypeCommandResult: true,
	audit.TypePhaseBegin: true, audit.TypePhaseEnd: true,
}

const (
	// indexFlushInterval is how often collected index entries are saved.
	indexFlushInterval = 2 * time.Second
	// stepDownFlushTimeout bounds the last flush when the leader steps down.
	stepDownFlushTimeout = 5 * time.Second
	// indexDropStep is how many of the oldest index entries go at a time when a
	// record would outgrow its ConfigMap.
	indexDropStep = 50
)

// auditIndex collects indexed entries as the writer appends them, until a flush
// saves them into each Intent's record. It is best-effort: entries collected but
// not yet saved are lost if the process stops.
type auditIndex struct {
	mu      sync.Mutex
	pending map[string][]IndexEntry
}

func (x *auditIndex) observe(e audit.Entry) {
	if e.Intent == "" || !indexedTypes[e.Type] {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.pending == nil {
		x.pending = map[string][]IndexEntry{}
	}
	x.pending[e.Intent] = append(x.pending[e.Intent], IndexEntry{Seq: e.Seq, Type: e.Type})
}

func (x *auditIndex) take(intent string) []IndexEntry {
	x.mu.Lock()
	defer x.mu.Unlock()
	es := x.pending[intent]
	delete(x.pending, intent)
	return es
}

func (x *auditIndex) restore(intent string, es []IndexEntry) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.pending == nil {
		x.pending = map[string][]IndexEntry{}
	}
	x.pending[intent] = append(es, x.pending[intent]...)
}

func (x *auditIndex) intents() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	keys := make([]string, 0, len(x.pending))
	for k := range x.pending {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// observeWrites installs the audit writer's write hook: metrics and the
// QueryAudit index. The hook runs under the writer's mutex, so it only records.
func (g *Gateway) observeWrites(w *audit.Writer) {
	w.SetOnWrite(func(e audit.Entry) {
		g.metrics.ObserveWrite(e)
		g.index.observe(e)
	})
}

// flushIndex saves an Intent's collected index entries into its record, waiting
// for the Intent's lock. QueryAudit uses it: read-your-writes needs the wait.
func (g *Gateway) flushIndex(ctx context.Context, intent string) error {
	// Lock before take: a flush that has taken entries holds the lock until they
	// are saved, so a concurrent QueryAudit waits and then reads them (read-your-writes).
	unlock := g.locks.lock(intent)
	defer unlock()
	return g.saveIndexLocked(ctx, intent)
}

// saveIndexLocked saves an Intent's collected index entries into its record; the
// caller holds the Intent's lock. When the record would outgrow its ConfigMap,
// the oldest index entries go first. On failure the entries stay pending.
func (g *Gateway) saveIndexLocked(ctx context.Context, intent string) error {
	es := g.index.take(intent)
	if len(es) == 0 {
		return nil
	}
	rec, err := g.state.LoadAny(ctx, intent)
	if err != nil {
		g.index.restore(intent, es)
		return err
	}
	if rec.cm == nil {
		uid, drop, err := g.afterTeardown(ctx, intent)
		if err != nil {
			g.index.restore(intent, es)
			return err
		}
		if drop { // the audit log keeps them
			return nil
		}
		if uid != "" {
			rec.IntentUID = uid
		}
	}
	rec.AddIndex(es)
	if err := g.saveRecord(ctx, rec); err != nil {
		g.index.restore(intent, es)
		return err
	}
	if gone, ok := g.tornDown.Load(intent); ok && gone != rec.IntentUID && rec.IntentUID != "" {
		// A recreated Intent owns the record now; forget its predecessor's teardown.
		g.tornDown.CompareAndDelete(intent, gone)
	}
	return nil
}

// afterTeardown decides who owns the entries a flush holds for an Intent without
// a record. When a succeeded Delete dropped that record, drop is true while the
// torn-down Intent, or no Intent, holds the name: saving the entries would bring
// the record back. A recreated Intent's UID comes back as uid, to own the new
// record; the caller forgets the teardown once that record is saved.
func (g *Gateway) afterTeardown(ctx context.Context, intent string) (uid string, drop bool, err error) {
	gone, ok := g.tornDown.Load(intent)
	if !ok {
		return "", false, nil
	}
	ns, name, _ := strings.Cut(intent, "/")
	var in lastv1alpha1.Intent
	switch err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &in); {
	case apierrors.IsNotFound(err):
		return "", true, nil
	case err != nil:
		return "", false, fmt.Errorf("gateway: read intent %s: %w", intent, err)
	case string(in.UID) == gone:
		return "", true, nil
	}
	return string(in.UID), false, nil
}

// saveRecord saves rec. The QueryAudit index is best-effort: when the record would
// outgrow its ConfigMap, the oldest index entries go first, so phase state always
// fits. The executed plan's pinned entries never go.
func (g *Gateway) saveRecord(ctx context.Context, rec *LoadedRecord) error {
	for {
		err := g.state.Save(ctx, rec)
		if errors.Is(err, errRecordTooLarge) && rec.DropIndex(indexDropStep) > 0 {
			continue
		}
		return err
	}
}

// flushIndexes saves every Intent's collected entries, and the image index's
// lastUsed times when due; failures are logged and retried on the next flush. It runs in the leader loop, so it never waits for a
// busy Intent: EndPhase can hold an Intent's lock for as long as a sandbox step
// runs. A skipped Intent's entries stay pending for the next flush or a QueryAudit.
func (g *Gateway) flushIndexes(ctx context.Context) {
	for _, intent := range g.index.intents() {
		unlock, ok := g.locks.tryLock(intent)
		if !ok {
			continue
		}
		err := g.saveIndexLocked(ctx, intent)
		unlock()
		if err != nil {
			slog.Warn("Could not save the audit index", "intent", intent, "err", err)
		}
	}
	if err := g.images.flushUsed(ctx, g.builds.now()); err != nil {
		slog.Warn("Could not save the image index", "err", err)
	}
}

// flushIndexesOnStepDown is the leader's last flush, after the drain. lead's
// context is already canceled by then, so it runs detached from it, bounded by
// stepDownFlushTimeout. It skips busy Intents like the periodic flush: after a
// drain that finished nothing holds an Intent's lock, and after one that timed
// out a stuck RPC might, and waiting for it would stall the step-down.
func (g *Gateway) flushIndexesOnStepDown(ctx context.Context) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepDownFlushTimeout)
	defer cancel()
	g.flushIndexes(fctx)
}
