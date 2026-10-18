package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)} }

func writerConfig(c *fakeClock) audit.WriterConfig {
	return audit.WriterConfig{
		ChainID: chainID, Retention: time.Hour,
		CheckpointEvery: 1000, CheckpointInterval: 10 * time.Minute,
		PutAttempts: 3, RetryBackoff: time.Millisecond,
		Now: c.Now, Actor: actorGateway,
	}
}

func openWriter(t *testing.T, sink audit.Sink, s *audit.Signer, cfg audit.WriterConfig) *audit.Writer {
	t.Helper()
	w, err := audit.Open(bg, sink, s, cfg, func(audit.StartInfo) any { return map[string]string{"pod": testValue} })
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return w
}

func readEntry(t *testing.T, sink audit.Sink, seq uint64) audit.Entry {
	t.Helper()
	body, err := sink.Get(bg, audit.EntryKey(chainID, seq), "")
	if err != nil {
		t.Fatalf("get seq %d: %v", seq, err)
	}
	var e audit.Entry
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func cmd(n int) audit.Draft {
	return audit.Draft{Type: audit.TypeCommandStart, Actor: actorOperator, Intent: intentTeamXWeb, Payload: map[string]int{"n": n}}
}

func TestOpenEmptyChainStartsAtGenesis(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	openWriter(t, sink, s, writerConfig(newClock()))
	e := readEntry(t, sink, 0)
	if e.Type != audit.TypeGatewayStart || e.PrevHash != audit.GenesisHash || e.Actor != actorGateway {
		t.Fatalf("first entry = %+v", e)
	}
	if e.TS != "2026-09-27T12:00:00.000000000Z" {
		t.Fatalf("ts = %s", e.TS)
	}
}

func TestAppendChainsAndVerifies(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	for i := range 10 {
		if _, err := w.Append(bg, cmd(i)); err != nil {
			t.Fatal(err)
		}
	}
	rep := verifyChain(t, sink, s, audit.VerifyOptions{})
	if !rep.OK() || rep.Head.Seq != 10 {
		t.Fatalf("report = %+v", rep)
	}
	if seq, hash, ok := w.Head(); !ok || seq != 10 || hash != rep.Head.Hash {
		t.Fatalf("Head() = %d %s %v", seq, hash, ok)
	}
}

func TestOpenResumesAfterHead(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	w := openWriter(t, sink, s, cfg)
	for i := range 3 {
		_, _ = w.Append(bg, cmd(i))
	}
	openWriter(t, sink, s, cfg)
	e := readEntry(t, sink, 4)
	if e.Type != audit.TypeGatewayStart || e.PrevHash != readEntry(t, sink, 3).Hash {
		t.Fatalf("resumed entry = %+v", e)
	}
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("problems: %+v", rep.Problems)
	}
}

func TestOpenRefusesTamperedChain(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	w := openWriter(t, sink, s, cfg)
	for i := range 5 {
		_, _ = w.Append(bg, cmd(i))
	}
	sink.TamperReplace(audit.EntryKey(chainID, 2), []byte("garbage"))
	_, err := audit.Open(bg, sink, s, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "seq 2") {
		t.Fatalf("open on tampered chain = %v, want refusal naming seq 2", err)
	}
}

func TestCheckpointEveryN(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	cfg.CheckpointEvery = 5
	w := openWriter(t, sink, s, cfg) // seq 0
	for i := range 4 {               // seq 1..4, then checkpoint at 5
		_, _ = w.Append(bg, cmd(i))
	}
	cp := readEntry(t, sink, 5)
	if cp.Type != audit.TypeCheckpoint {
		t.Fatalf("seq 5 type = %s, want checkpoint", cp.Type)
	}
	var p audit.CheckpointPayload
	_ = json.Unmarshal(cp.Payload, &p)
	if p.PrevSeq != 4 || p.PrevHash != readEntry(t, sink, 4).Hash {
		t.Fatalf("checkpoint payload = %+v", p)
	}
	if _, err := sink.Get(bg, audit.CheckpointKey(chainID, 5), ""); err != nil {
		t.Fatalf("checkpoint index missing: %v", err)
	}
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("problems: %+v", rep.Problems)
	}
}

func TestCheckpointIfDueUsesInterval(t *testing.T) {
	s, sink, clk := newSigner(t), audittest.NewMemSink(), newClock()
	w := openWriter(t, sink, s, writerConfig(clk)) // seq 0
	if err := w.CheckpointIfDue(bg); err != nil {
		t.Fatal(err)
	}
	if len(sink.Keys()) != 1 {
		t.Fatalf("checkpoint written before the interval elapsed: %v", sink.Keys())
	}
	clk.Advance(10 * time.Minute)
	if err := w.CheckpointIfDue(bg); err != nil {
		t.Fatal(err)
	}
	if readEntry(t, sink, 1).Type != audit.TypeCheckpoint {
		t.Fatal("no checkpoint after the interval")
	}
	clk.Advance(time.Hour)
	_ = w.CheckpointIfDue(bg)
	if _, err := sink.Get(bg, audit.EntryKey(chainID, 2), ""); err == nil {
		t.Fatal("checkpoint written with nothing new since the last one")
	}
}

func TestOpenVerifiesFromLatestCheckpoint(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	cfg.CheckpointEvery = 4
	w := openWriter(t, sink, s, cfg) // seq 0
	for i := range 5 {               // seq 1,2,3 -> checkpoint 4 -> seq 5,6
		_, _ = w.Append(bg, cmd(i))
	}
	if readEntry(t, sink, 4).Type != audit.TypeCheckpoint {
		t.Fatal("expected checkpoint at seq 4")
	}
	sink.TamperReplace(audit.EntryKey(chainID, 1), []byte("garbage"))
	if _, err := audit.Open(bg, sink, s, cfg, nil); err != nil {
		t.Fatalf("entries before the latest checkpoint are trusted at startup, got: %v", err)
	}
	sink.TamperReplace(audit.EntryKey(chainID, 5), []byte("garbage"))
	if _, err := audit.Open(bg, sink, s, cfg, nil); err == nil {
		t.Fatal("open accepted tampering after the latest checkpoint")
	}
}

func TestPersistentPutFailureBreaksWriter(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	var failures int
	w.SetOnFailure(func(error) { failures++ })
	sink.SetFailPuts(true)
	if _, err := w.Append(bg, cmd(1)); !errors.Is(err, audit.ErrWriterBroken) {
		t.Fatalf("append = %v, want ErrWriterBroken", err)
	}
	sink.SetFailPuts(false)
	if _, err := w.Append(bg, cmd(2)); !errors.Is(err, audit.ErrWriterBroken) {
		t.Fatalf("broken writer accepted an append: %v", err)
	}
	if !w.Broken() || failures != 1 {
		t.Fatalf("broken=%v failures=%d", w.Broken(), failures)
	}
}

// flakySink stores the object but reports failure once, like a timeout after the write landed.
type flakySink struct {
	*audittest.MemSink
	failed bool
}

func (f *flakySink) Put(ctx context.Context, key string, body []byte, until time.Time) error {
	if err := f.MemSink.Put(ctx, key, body, until); err != nil {
		return err
	}
	if !f.failed {
		f.failed = true
		return errors.New("timeout after write")
	}
	return nil
}

func TestAmbiguousWriteIsRecovered(t *testing.T) {
	s := newSigner(t)
	sink := &flakySink{MemSink: audittest.NewMemSink()}
	w := openWriter(t, sink, s, writerConfig(newClock()))
	if w.Broken() {
		t.Fatal("writer broke on a write that actually landed")
	}
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("problems: %+v", rep.Problems)
	}
}

func TestChainConflictBreaksWriter(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	sink.TamperPut(audit.EntryKey(chainID, 1), []byte(`{"another":"writer"}`))
	if _, err := w.Append(bg, cmd(1)); !errors.Is(err, audit.ErrChainConflict) {
		t.Fatalf("append = %v, want ErrChainConflict", err)
	}
	if _, err := w.Append(bg, cmd(2)); !errors.Is(err, audit.ErrWriterBroken) {
		t.Fatalf("append after conflict = %v, want ErrWriterBroken", err)
	}
}

func after(before audit.Entry, result any, err error) audit.Draft {
	payload := map[string]any{"beforeSeq": before.Seq, "beforeHash": before.Hash, "result": result}
	if err != nil {
		payload["error"] = err.Error()
	}
	return audit.Draft{Type: audit.TypeCommandResult, Actor: actorGateway, Payload: payload}
}

func TestDoRecordsBeforeRunningAction(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	var sawBefore bool
	err := w.Do(bg, cmd(1), func(context.Context) (any, error) {
		_, getErr := sink.Get(bg, audit.EntryKey(chainID, 1), "")
		sawBefore = getErr == nil
		return map[string]int{"exitCode": 0}, nil
	}, after)
	if err != nil || !sawBefore {
		t.Fatalf("err=%v sawBefore=%v", err, sawBefore)
	}
	if readEntry(t, sink, 2).Type != audit.TypeCommandResult {
		t.Fatal("outcome not recorded")
	}
}

func TestDoRefusesActionWhenAuditUnavailable(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	sink.SetFailPuts(true)
	called := false
	err := w.Do(bg, cmd(1), func(context.Context) (any, error) { called = true; return nil, nil }, after)
	if !errors.Is(err, audit.ErrAuditUnavailable) || called {
		t.Fatalf("err=%v called=%v; want ErrAuditUnavailable and no action", err, called)
	}
}

func TestDoRecordsActionFailure(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	actionErr := errors.New("exit status 1")
	err := w.Do(bg, cmd(1), func(context.Context) (any, error) { return nil, actionErr }, after)
	if !errors.Is(err, actionErr) {
		t.Fatalf("Do = %v, want the action's error", err)
	}
	if !strings.Contains(string(readEntry(t, sink, 2).Payload), "exit status 1") {
		t.Fatal("failure not recorded in the outcome entry")
	}
}

// cancelSink wraps a sink and cancels its context during Put if the sink's context is already done.
type cancelSink struct {
	*audittest.MemSink
}

func (c *cancelSink) Put(ctx context.Context, key string, body []byte, until time.Time) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return c.MemSink.Put(context.Background(), key, body, until)
}

func (c *cancelSink) Get(ctx context.Context, key string, s string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return c.MemSink.Get(context.Background(), key, s)
}

func TestDoOutcomeRecordedDespiteContextCancellation(t *testing.T) {
	s := newSigner(t)
	sink := &cancelSink{MemSink: audittest.NewMemSink()}
	w := openWriter(t, sink, s, writerConfig(newClock()))

	// Create a context that will be cancelled
	ctx, cancel := context.WithCancel(context.Background())
	actionErr := errors.New("action error")

	err := w.Do(ctx, cmd(1), func(context.Context) (any, error) {
		// Cancel the context during the action, simulating a timeout or client disconnect
		cancel()
		return nil, actionErr
	}, after)

	// (a) the outcome entry is recorded
	if _, getErr := sink.Get(context.Background(), audit.EntryKey(chainID, 2), ""); getErr != nil {
		t.Fatalf("outcome entry not recorded: %v", getErr)
	}

	// (b) Do returns the action's error
	if !errors.Is(err, actionErr) {
		t.Fatalf("Do = %v, want the action's error", err)
	}

	// (c) w.Broken() is false
	if w.Broken() {
		t.Fatal("writer should not be broken after context cancellation during action")
	}

	// (d) a later Append succeeds
	if _, appendErr := w.Append(context.Background(), cmd(3)); appendErr != nil {
		t.Fatalf("append after context cancellation = %v, want success", appendErr)
	}

	// (e) the chain still verifies
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("chain verification failed: %+v", rep.Problems)
	}
}

func TestOpenRefusesZeroRetention(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	cfg.Retention = 0
	_, err := audit.Open(bg, sink, s, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "Retention must be positive") {
		t.Fatalf("open with Retention 0 = %v, want error about Retention", err)
	}
	// Ensure no entry was written
	if len(sink.Keys()) != 0 {
		t.Fatalf("open with Retention 0 wrote entries, want none: %v", sink.Keys())
	}
}

// F6: the Writer stores the RFC 8785 canonical bytes of a version-1 entry.
func TestWriterStoresCanonicalVersion1Body(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	if _, err := w.Append(bg, cmd(1)); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(0); seq <= 1; seq++ {
		body, err := sink.Get(bg, audit.EntryKey(chainID, seq), "")
		if err != nil {
			t.Fatal(err)
		}
		e := readEntry(t, sink, seq)
		if e.V != 1 {
			t.Fatalf("seq %d: v = %d, want 1", seq, e.V)
		}
		if want := mustCanonical(t, e); !bytes.Equal(body, want) {
			t.Fatalf("seq %d: stored body is not canonical:\n got %s\nwant %s", seq, body, want)
		}
	}
}

// F6: numbers that RFC 8785 would alter (non-integral, or |n| > 2^53) must be
// rejected, because the signed canonical form would differ from what the caller meant.
func TestAppendRejectsNumbersCanonicalJSONWouldAlter(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	for name, payload := range map[string]any{
		"int above 2^53":       map[string]uint64{"n": 9007199254740993},
		"negative below -2^53": map[string]int64{"n": -9007199254740993},
		"fraction":             map[string]float64{"n": 0.5},
		"nested":               map[string]any{"a": []any{1, map[string]uint64{"b": 1 << 60}}},
		"raw exponent":         json.RawMessage(`{"n":1e3}`),
		"raw big":              json.RawMessage(`[18446744073709551615]`),
	} {
		_, err := w.Append(bg, audit.Draft{Type: audit.TypeCommandStart, Actor: actorOperator, Payload: payload})
		if err == nil || !strings.Contains(err.Error(), "encode it as a string") {
			t.Errorf("%s: Append = %v, want a rejection telling the caller to use a string", name, err)
		}
	}
	if w.Broken() {
		t.Fatal("a rejected payload must not break the writer")
	}
	for name, payload := range map[string]any{
		"small int": map[string]int{"n": 12345},
		"2^53":      map[string]uint64{"n": 1 << 53},
		"-2^53":     map[string]int64{"n": -(1 << 53)},
		"string":    map[string]string{"n": "9007199254740993"},
		"raw":       json.RawMessage(`{"n":-7,"s":"1.5"}`),
	} {
		if _, err := w.Append(bg, audit.Draft{Type: audit.TypeCommandStart, Actor: actorOperator, Payload: payload}); err != nil {
			t.Errorf("%s: Append = %v, want success", name, err)
		}
	}
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() || rep.Head.Seq != 5 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestDoRefusesActionWithUnsafeNumberInBefore(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	before := audit.Draft{Type: audit.TypeCommandStart, Actor: actorOperator, Payload: map[string]uint64{"n": 9007199254740993}}
	called := false
	err := w.Do(bg, before, func(context.Context) (any, error) { called = true; return nil, nil }, after)
	if !errors.Is(err, audit.ErrAuditUnavailable) || called {
		t.Fatalf("err=%v called=%v; want ErrAuditUnavailable and no action", err, called)
	}
}

// F7: after receives the durable before-entry so the outcome can reference it.
func TestDoPassesDurableBeforeEntryToAfter(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	var got audit.Entry
	err := w.Do(bg, cmd(1), func(context.Context) (any, error) { return nil, nil },
		func(before audit.Entry, result any, actionErr error) audit.Draft {
			got = before
			return after(before, result, actionErr)
		})
	if err != nil {
		t.Fatal(err)
	}
	stored := readEntry(t, sink, 1)
	if got.Seq != 1 || got.Hash == "" || got.Hash != stored.Hash || got.Type != audit.TypeCommandStart {
		t.Fatalf("after got before=%+v, want the stored seq-1 entry %+v", got, stored)
	}
	var p map[string]any
	_ = json.Unmarshal(readEntry(t, sink, 2).Payload, &p)
	if p["beforeHash"] != stored.Hash {
		t.Fatalf("outcome payload = %v, want it to reference the before-entry hash", p)
	}
}

// F7: concurrent appends must serialize into one contiguous, verifiable chain.
func TestConcurrentAppendsFormOneChain(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	cfg.CheckpointEvery = 100 // exercise checkpoints under contention too
	w := openWriter(t, sink, s, cfg)
	const goroutines, perG = 16, 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perG)
	for g := range goroutines {
		wg.Go(func() {
			for i := range perG {
				if _, err := w.Append(bg, cmd(g*perG+i)); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append: %v", err)
	}
	rep := verifyChain(t, sink, s, audit.VerifyOptions{})
	// 1 gateway.start + 800 appends + one checkpoint after every 100 non-checkpoint entries.
	const want = 1 + goroutines*perG + (1+goroutines*perG)/100
	if !rep.OK() || rep.Entries != want || rep.First.Seq != 0 || rep.Head.Seq != want-1 {
		t.Fatalf("entries=%d head=%v problems=%+v; want %d contiguous entries", rep.Entries, rep.Head, rep.Problems, want)
	}
	seen := map[int]bool{}
	for seq := range uint64(want) {
		e := readEntry(t, sink, seq)
		if e.Type != audit.TypeCommandStart {
			continue
		}
		var p map[string]int
		_ = json.Unmarshal(e.Payload, &p)
		seen[p["n"]] = true
	}
	if len(seen) != goroutines*perG {
		t.Fatalf("recorded %d distinct appends, want %d", len(seen), goroutines*perG)
	}
}

// F7: the outcome entry itself fails -> "outcome not recorded", action ran once, writer broken.
func TestDoReportsOutcomeNotRecorded(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	calls := 0
	err := w.Do(bg, cmd(1), func(context.Context) (any, error) {
		calls++
		sink.SetFailPuts(true) // the store fails after the before-entry landed
		return nil, nil
	}, after)
	if err == nil || !strings.Contains(err.Error(), "outcome not recorded") || strings.Contains(err.Error(), "outcome recorded") {
		t.Fatalf("Do = %v, want an error saying the outcome was not recorded", err)
	}
	if errors.Is(err, audit.ErrCheckpointFailed) {
		t.Fatalf("Do = %v; the outcome entry itself failed, not a checkpoint", err)
	}
	if calls != 1 || !w.Broken() {
		t.Fatalf("calls=%d broken=%v; want the action run once and the writer broken", calls, w.Broken())
	}
	if _, getErr := sink.Get(bg, audit.EntryKey(chainID, 2), ""); getErr == nil {
		t.Fatal("an outcome entry exists although Do said it was not recorded")
	}
}

// failKeySink fails every Put to one key.
type failKeySink struct {
	*audittest.MemSink
	key string
}

func (f *failKeySink) Put(ctx context.Context, key string, body []byte, until time.Time) error {
	if key == f.key {
		return errors.New("store unavailable")
	}
	return f.MemSink.Put(ctx, key, body, until)
}

// F7: the outcome entry lands but the checkpoint after it fails -> Do must say the
// outcome WAS recorded, and Append reports ErrCheckpointFailed with the stored entry.
func TestDoReportsOutcomeRecordedButCheckpointFailed(t *testing.T) {
	s := newSigner(t)
	sink := &failKeySink{MemSink: audittest.NewMemSink(), key: audit.EntryKey(chainID, 3)}
	cfg := writerConfig(newClock())
	cfg.CheckpointEvery = 3 // gateway.start(0), before(1), outcome(2) -> checkpoint due at seq 3
	w := openWriter(t, sink, s, cfg)
	calls := 0
	err := w.Do(bg, cmd(1), func(context.Context) (any, error) { calls++; return nil, nil }, after)
	if err == nil || !strings.Contains(err.Error(), "outcome recorded but checkpoint failed") || strings.Contains(err.Error(), "outcome not recorded") {
		t.Fatalf("Do = %v, want an error saying the outcome was recorded but the checkpoint failed", err)
	}
	if !errors.Is(err, audit.ErrCheckpointFailed) {
		t.Fatalf("Do = %v, want it to wrap ErrCheckpointFailed", err)
	}
	if readEntry(t, sink, 2).Type != audit.TypeCommandResult || calls != 1 || !w.Broken() {
		t.Fatalf("calls=%d broken=%v; want the outcome stored, the action run once, and the writer broken", calls, w.Broken())
	}
}

func TestAppendReturnsStoredEntryWhenCheckpointFails(t *testing.T) {
	s := newSigner(t)
	sink := &failKeySink{MemSink: audittest.NewMemSink(), key: audit.EntryKey(chainID, 2)}
	cfg := writerConfig(newClock())
	cfg.CheckpointEvery = 2 // gateway.start(0), append(1) -> checkpoint due at seq 2
	w := openWriter(t, sink, s, cfg)
	e, err := w.Append(bg, cmd(1))
	if !errors.Is(err, audit.ErrCheckpointFailed) {
		t.Fatalf("Append = %v, want ErrCheckpointFailed", err)
	}
	if e.Seq != 1 || e.Hash != readEntry(t, sink, 1).Hash {
		t.Fatalf("Append returned %+v, want the stored seq-1 entry", e)
	}
}

// F3: a new (empty) chain must not be started silently in a bucket that already
// holds another chain — e.g. after the chain-ID ConfigMap was lost or re-rendered.
func TestOpenRefusesNewChainWhenBucketHoldsAnotherChain(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	for _, other := range []string{"old-chain-a", "old-chain-b"} {
		cfg := writerConfig(newClock())
		cfg.ChainID, cfg.AllowNewChain = other, true
		openWriter(t, sink, s, cfg)
	}
	before := sink.Keys()

	_, err := audit.Open(bg, sink, s, writerConfig(newClock()), nil)
	if err == nil || !strings.Contains(err.Error(), "old-chain-a") || !strings.Contains(err.Error(), "old-chain-b") ||
		!strings.Contains(err.Error(), "LAST_AUDIT_ALLOW_NEW_CHAIN=true") {
		t.Fatalf("Open = %v, want a refusal naming the existing chains and the override", err)
	}
	if after := sink.Keys(); len(after) != len(before) {
		t.Fatalf("refused Open wrote entries: %v", after)
	}

	cfg := writerConfig(newClock())
	cfg.AllowNewChain = true
	openWriter(t, sink, s, cfg)
	if readEntry(t, sink, 0).Type != audit.TypeGatewayStart {
		t.Fatal("AllowNewChain did not start the new chain")
	}
	// Once the chain exists, reopening it is a resumption and needs no override.
	openWriter(t, sink, s, writerConfig(newClock()))
}

func TestOpenStartsNewChainInEmptyBucket(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	cfg.AllowNewChain = false
	openWriter(t, sink, s, cfg)
	if keys := sink.Keys(); len(keys) != 1 || keys[0] != audit.EntryKey(chainID, 0) {
		t.Fatalf("keys = %v", keys)
	}
}

// F3: Open tells the start-payload builder which verified head it resumed from.
func TestOpenReportsResumedFrom(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	var infos []audit.StartInfo
	record := func(info audit.StartInfo) any {
		infos = append(infos, info)
		return map[string]any{"resumedFrom": info.ResumedFrom}
	}
	w, err := audit.Open(bg, sink, s, cfg, record)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Append(bg, cmd(1))
	if _, err := audit.Open(bg, sink, s, cfg, record); err != nil {
		t.Fatal(err)
	}
	head := readEntry(t, sink, 1)
	if len(infos) != 2 || infos[0].ResumedFrom != nil || infos[1].ResumedFrom == nil ||
		*infos[1].ResumedFrom != (audit.ChainHead{Seq: 1, Hash: head.Hash}) {
		t.Fatalf("start infos = %+v, want nil then {1 %s}", infos, head.Hash)
	}
	if got, want := string(readEntry(t, sink, 2).Payload), fmt.Sprintf(`{"resumedFrom":{"hash":%q,"seq":1}}`, head.Hash); got != want {
		t.Fatalf("gateway.start payload = %s, want %s", got, want)
	}
}

// DoEntry returns the stored outcome entry, so a caller can point at it.
func TestDoEntryReturnsTheStoredOutcome(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	got, err := w.DoEntry(bg, cmd(1), func(context.Context) (any, error) { return nil, nil }, after)
	if err != nil {
		t.Fatal(err)
	}
	if stored := readEntry(t, sink, 2); got.Seq != 2 || got.Hash != stored.Hash || got.Type != audit.TypeCommandResult {
		t.Fatalf("DoEntry = %+v, want the stored seq-2 outcome %+v", got, stored)
	}
	sink.SetFailPuts(true)
	if got, err := w.DoEntry(bg, cmd(2), func(context.Context) (any, error) { return nil, nil }, after); err == nil || got.Seq != 0 || got.Hash != "" {
		t.Fatalf("DoEntry with a failing store = %+v, %v; want the zero entry and an error", got, err)
	}
}
