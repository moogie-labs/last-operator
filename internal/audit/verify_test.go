package audit_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

const chainID = "test-chain"

var bg = context.Background()

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func putEntry(t *testing.T, sink *audittest.MemSink, e audit.Entry) {
	t.Helper()
	if err := sink.Put(bg, audit.EntryKey(chainID, e.Seq), mustMarshal(t, e), time.Time{}); err != nil {
		t.Fatal(err)
	}
}

// buildChain seals n entries (seq 0..n-1) straight into sink, bypassing Writer.
// mutate, if non-nil, may alter each entry (PrevHash is already set) before sealing.
func buildChain(t *testing.T, sink *audittest.MemSink, s *audit.Signer, n int, mutate func(*audit.Entry)) []audit.Entry {
	t.Helper()
	prev := audit.GenesisHash
	out := make([]audit.Entry, 0, n)
	for i := 0; i < n; i++ {
		e := audit.Entry{
			Seq: uint64(i), ChainID: chainID, PrevHash: prev,
			TS: "2026-09-27T00:00:00.000000000Z", Type: audit.TypeCommandStart,
			Actor: "operator", Intent: "team-x/web",
			Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
		}
		if mutate != nil {
			mutate(&e)
		}
		if err := s.Seal(&e); err != nil {
			t.Fatal(err)
		}
		putEntry(t, sink, e)
		prev = e.Hash
		out = append(out, e)
	}
	return out
}

func verifyChain(t *testing.T, sink audit.Sink, s *audit.Signer, opts audit.VerifyOptions) audit.Report {
	t.Helper()
	rep, err := audit.Verify(bg, sink, s.Public(), chainID, opts)
	if err != nil {
		t.Fatalf("verify returned an error: %v", err)
	}
	return rep
}

func requireProblem(t *testing.T, rep audit.Report, seq uint64, kind audit.ProblemKind) {
	t.Helper()
	p, ok := rep.FirstBad()
	if !ok {
		t.Fatalf("expected %s at seq %d, but the chain verified clean", kind, seq)
	}
	if p.Seq != seq || p.Kind != kind {
		t.Fatalf("first problem = %+v, want %s at seq %d (all: %+v)", p, kind, seq, rep.Problems)
	}
}

func TestVerifyCleanChain(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	rep := verifyChain(t, sink, s, audit.VerifyOptions{})
	if !rep.OK() || rep.Entries != 10 || rep.First.Seq != 0 || rep.Head.Seq != 9 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifyEmptyChain(t *testing.T) {
	s := newSigner(t)
	rep := verifyChain(t, audittest.NewMemSink(), s, audit.VerifyOptions{})
	if !rep.OK() || rep.Entries != 0 || rep.Head != nil {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifyDetectsDeletedEntry(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperDelete(audit.EntryKey(chainID, 3))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 3, audit.ProblemGap)
}

func TestVerifyDetectsReorderedEntries(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, nil)
	sink.TamperReplace(audit.EntryKey(chainID, 2), mustMarshal(t, es[3]))
	sink.TamperReplace(audit.EntryKey(chainID, 3), mustMarshal(t, es[2]))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 2, audit.ProblemSeqMismatch)
}

func TestVerifyDetectsModifiedPayload(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, nil)
	e := es[4]
	e.Payload = json.RawMessage(`{"n":999}`)
	sink.TamperReplace(audit.EntryKey(chainID, 4), mustMarshal(t, e))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 4, audit.ProblemBadHash)
}

func TestVerifyDetectsInvalidSignature(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, nil)
	e := es[5]
	e.Sig = "ed25519:" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	sink.TamperReplace(audit.EntryKey(chainID, 5), mustMarshal(t, e))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemBadSignature)
}

func TestVerifyDetectsEntryResignedWithAnotherKey(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, nil)
	e := es[6]
	e.Payload = json.RawMessage(`{"n":"forged"}`)
	if err := newSigner(t).Seal(&e); err != nil {
		t.Fatal(err)
	}
	sink.TamperReplace(audit.EntryKey(chainID, 6), mustMarshal(t, e))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 6, audit.ProblemBadSignature)
}

func TestVerifyDetectsBrokenLink(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, func(e *audit.Entry) {
		if e.Seq == 4 {
			e.PrevHash = "sha256:" + strings.Repeat("f", 64)
		}
	})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 4, audit.ProblemBrokenLink)
}

func TestVerifyDetectsBadGenesis(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 3, func(e *audit.Entry) {
		if e.Seq == 0 {
			e.PrevHash = "sha256:" + strings.Repeat("1", 64)
		}
	})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 0, audit.ProblemBrokenLink)
}

func TestVerifyDetectsExtraVersion(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperAddVersion(audit.EntryKey(chainID, 5), []byte(`{}`))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemExtraVersion)
}

func TestVerifyDetectsDeleteMarker(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperAddDeleteMarker(audit.EntryKey(chainID, 7))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 7, audit.ProblemDeleteMarker)
}

func TestVerifyDetectsMalformedEntry(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 5, nil)
	sink.TamperReplace(audit.EntryKey(chainID, 2), []byte("not json"))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 2, audit.ProblemMalformed)
}

func TestVerifyRejectsUnknownFields(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 5, nil)
	raw := strings.TrimSuffix(string(mustMarshal(t, es[1])), "}") + `,"evil":true}`
	sink.TamperReplace(audit.EntryKey(chainID, 1), []byte(raw))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 1, audit.ProblemMalformed)
}

func checkpointAt(seq uint64) func(*audit.Entry) {
	return func(e *audit.Entry) {
		if e.Seq == seq {
			e.Type = audit.TypeCheckpoint
			b, _ := json.Marshal(audit.CheckpointPayload{PrevSeq: seq - 1, PrevHash: e.PrevHash})
			e.Payload = b
		}
	}
}

func TestVerifyAcceptsConsistentCheckpoint(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, checkpointAt(5))
	_ = sink.Put(bg, audit.CheckpointKey(chainID, 5), mustMarshal(t, audit.CheckpointIndex{Seq: 5, Hash: es[5].Hash}), time.Time{})
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("problems: %+v", rep.Problems)
	}
}

func TestVerifyDetectsInconsistentCheckpointPayload(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, func(e *audit.Entry) {
		if e.Seq == 5 {
			e.Type = audit.TypeCheckpoint
			b, _ := json.Marshal(audit.CheckpointPayload{PrevSeq: 3, PrevHash: "sha256:x"})
			e.Payload = b
		}
	})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemBadCheckpoint)
}

func TestVerifyDetectsForgedCheckpointIndex(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	forged := audit.CheckpointIndex{Seq: 3, Hash: "sha256:" + strings.Repeat("a", 64)}
	_ = sink.Put(bg, audit.CheckpointKey(chainID, 3), mustMarshal(t, forged), time.Time{})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 3, audit.ProblemBadCheckpoint)
}

func TestVerifyDetectsCheckpointIndexBeyondHead(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	_ = sink.Put(bg, audit.CheckpointKey(chainID, 42), mustMarshal(t, audit.CheckpointIndex{Seq: 42, Hash: "sha256:x"}), time.Time{})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 42, audit.ProblemBadCheckpoint)
}

func TestVerifyDetectsTruncatedTailOnlyWithExpectation(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperDelete(audit.EntryKey(chainID, 9))
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("without an expectation, tail truncation is undetectable by design; got %+v", rep.Problems)
	}
	want := uint64(9)
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{ExpectHeadAtLeast: &want}), 9, audit.ProblemTruncated)
}

func TestVerifyFromSeqSkipsEarlierEntries(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperReplace(audit.EntryKey(chainID, 2), []byte("garbage"))
	rep := verifyChain(t, sink, s, audit.VerifyOptions{FromSeq: 5})
	if !rep.OK() || rep.Entries != 5 || rep.First.Seq != 5 {
		t.Fatalf("report = %+v", rep)
	}
}

// Regression tests for Finding 1: canonical encoding validation.

func TestVerifyDetectsTrailingDataAfterEntry(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 5, nil)
	// Append trailing junk to the entry body.
	e := es[2]
	body := mustMarshal(t, e)
	body = append(body, []byte(`{"actor":"attacker"}`)...)
	sink.TamperReplace(audit.EntryKey(chainID, 2), body)
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 2, audit.ProblemMalformed)
}

func TestVerifyDetectsNonCanonicalEncoding(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 5, nil)
	// Add a duplicate key with different casing.
	raw := strings.TrimSuffix(string(mustMarshal(t, es[1])), "}") + `,"ACTOR":"x"}`
	sink.TamperReplace(audit.EntryKey(chainID, 1), []byte(raw))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 1, audit.ProblemMalformed)
}

// Focused tests for Finding 2: checkpoint validation conditions.

func TestVerifyDetectsCheckpointIndexWithWrongHash(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, checkpointAt(5))
	// Create checkpoint index with wrong hash but correct seq, pointing at a checkpoint entry.
	wrongHash := audit.CheckpointIndex{Seq: 5, Hash: "sha256:" + strings.Repeat("b", 64)}
	if err := sink.Put(bg, audit.CheckpointKey(chainID, 5), mustMarshal(t, wrongHash), time.Time{}); err != nil {
		t.Fatal(err)
	}
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemBadCheckpoint)
}

func TestVerifyDetectsCheckpointIndexOfNonCheckpointEntry(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, nil)
	// Create checkpoint index with correct hash of a non-checkpoint entry (seq 5 is NOT a checkpoint).
	idx := audit.CheckpointIndex{Seq: 5, Hash: es[5].Hash}
	if err := sink.Put(bg, audit.CheckpointKey(chainID, 5), mustMarshal(t, idx), time.Time{}); err != nil {
		t.Fatal(err)
	}
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemBadCheckpoint)
}

func TestVerifyDetectsCheckpointIndexSeqMismatch(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, checkpointAt(5))
	// Create checkpoint index whose body seq differs from its key.
	mismatchIdx := audit.CheckpointIndex{Seq: 7, Hash: es[5].Hash}
	if err := sink.Put(bg, audit.CheckpointKey(chainID, 5), mustMarshal(t, mismatchIdx), time.Time{}); err != nil {
		t.Fatal(err)
	}
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemBadCheckpoint)
}

func TestVerifyDetectsCheckpointPayloadWithWrongPrevHash(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, func(e *audit.Entry) {
		if e.Seq == 5 {
			e.Type = audit.TypeCheckpoint
			// Correct PrevSeq but wrong PrevHash.
			b, _ := json.Marshal(audit.CheckpointPayload{PrevSeq: 4, PrevHash: "sha256:" + strings.Repeat("x", 64)})
			e.Payload = b
		}
	})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemBadCheckpoint)
}
