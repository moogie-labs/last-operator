package audit_test

import (
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
		Now: c.Now, Actor: "gateway",
	}
}

func openWriter(t *testing.T, sink audit.Sink, s *audit.Signer, cfg audit.WriterConfig) *audit.Writer {
	t.Helper()
	w, err := audit.Open(bg, sink, s, cfg, map[string]string{"pod": "test"})
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
	return audit.Draft{Type: audit.TypeCommandStart, Actor: "operator", Intent: "team-x/web", Payload: map[string]int{"n": n}}
}

func TestOpenEmptyChainStartsAtGenesis(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	openWriter(t, sink, s, writerConfig(newClock()))
	e := readEntry(t, sink, 0)
	if e.Type != audit.TypeGatewayStart || e.PrevHash != audit.GenesisHash || e.Actor != "gateway" {
		t.Fatalf("first entry = %+v", e)
	}
	if e.TS != "2026-09-27T12:00:00.000000000Z" {
		t.Fatalf("ts = %s", e.TS)
	}
}

func TestAppendChainsAndVerifies(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	for i := 0; i < 10; i++ {
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
	for i := 0; i < 3; i++ {
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
	for i := 0; i < 5; i++ {
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
	for i := 0; i < 4; i++ {         // seq 1..4, then checkpoint at 5
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
	for i := 0; i < 5; i++ {         // seq 1,2,3 -> checkpoint 4 -> seq 5,6
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
	return audit.Draft{Type: audit.TypeCommandResult, Actor: "gateway", Payload: payload}
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
