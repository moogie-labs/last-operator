package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	w, err := audit.Open(bg, sink, s, cfg, map[string]string{"pod": testValue})
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

func after(result any, err error) audit.Draft {
	payload := map[string]any{"result": result}
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
