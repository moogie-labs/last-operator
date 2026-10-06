package gateway

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

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

// flushIndex saves an Intent's collected index entries into its record. When
// the record would outgrow its ConfigMap, the oldest index entries go first.
func (g *Gateway) flushIndex(ctx context.Context, intent string) error {
	es := g.index.take(intent)
	if len(es) == 0 {
		return nil
	}
	unlock := g.locks.lock(intent)
	defer unlock()
	rec, err := g.state.Load(ctx, intent)
	if err != nil {
		g.index.restore(intent, es)
		return err
	}
	rec.AddIndex(es)
	for {
		err := g.state.Save(ctx, rec)
		if errors.Is(err, errRecordTooLarge) && len(rec.Index) > 0 {
			rec.DropIndex(indexDropStep)
			continue
		}
		if err != nil {
			g.index.restore(intent, es)
		}
		return err
	}
}

// flushIndexes saves every Intent's collected entries; failures are logged and
// retried on the next flush.
func (g *Gateway) flushIndexes(ctx context.Context) {
	for _, intent := range g.index.intents() {
		if err := g.flushIndex(ctx, intent); err != nil {
			slog.Warn("Could not save the audit index", "intent", intent, "err", err)
		}
	}
}
