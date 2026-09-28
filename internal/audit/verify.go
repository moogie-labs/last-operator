package audit

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ProblemKind classifies a verification failure.
type ProblemKind string

const (
	ProblemGap           ProblemKind = "gap"
	ProblemMalformed     ProblemKind = "malformed"
	ProblemSeqMismatch   ProblemKind = "seq-mismatch"
	ProblemChainMismatch ProblemKind = "chain-mismatch"
	ProblemBadHash       ProblemKind = "bad-hash"
	ProblemBadSignature  ProblemKind = "bad-signature"
	ProblemBrokenLink    ProblemKind = "broken-link"
	ProblemExtraVersion  ProblemKind = "extra-version"
	ProblemDeleteMarker  ProblemKind = "delete-marker"
	ProblemBadCheckpoint ProblemKind = "bad-checkpoint"
	ProblemTruncated     ProblemKind = "truncated"
)

// Problem is one verification failure, anchored at a sequence number.
type Problem struct {
	Seq    uint64
	Kind   ProblemKind
	Detail string
}

// VerifyOptions narrows or strengthens verification.
type VerifyOptions struct {
	// FromSeq starts verification at this seq (normally a checkpoint).
	FromSeq uint64
	// ExpectHeadAtLeast, if set, fails verification when the chain ends before this seq.
	ExpectHeadAtLeast *uint64
}

// Report is the outcome of Verify.
type Report struct {
	Entries  uint64
	First    *Entry
	Head     *Entry
	Problems []Problem
}

func (r Report) OK() bool { return len(r.Problems) == 0 }

// FirstBad returns the problem with the lowest seq.
func (r Report) FirstBad() (Problem, bool) {
	if len(r.Problems) == 0 {
		return Problem{}, false
	}
	return r.Problems[0], true
}

func (r *Report) add(seq uint64, kind ProblemKind, format string, args ...any) {
	r.Problems = append(r.Problems, Problem{Seq: seq, Kind: kind, Detail: fmt.Sprintf(format, args...)})
}

// Verify checks the chain from opts.FromSeq to its head: sequence continuity,
// one version and no delete markers per entry, hashes, signatures, hash links,
// checkpoint payloads, and checkpoint index objects. It returns an error only
// when the store itself cannot be read.
//
//nolint:gocyclo // security-critical: each branch is a distinct chain-integrity check (spec §14.5); splitting it up would risk silently dropping a check during refactor.
func Verify(ctx context.Context, sink Sink, pub ed25519.PublicKey, chainID string, opts VerifyOptions) (Report, error) {
	var rep Report
	startAfter := ""
	if opts.FromSeq > 0 {
		startAfter = EntryKey(chainID, opts.FromSeq-1)
	}
	objs, err := sink.List(ctx, EntryPrefix(chainID), startAfter)
	if err != nil {
		return rep, fmt.Errorf("audit: list chain: %w", err)
	}

	hashes := map[uint64]string{}
	types := map[uint64]string{}
	expected := opts.FromSeq
	var prev *Entry
	for _, o := range objs {
		seq, err := SeqFromKey(o.Key)
		if err != nil {
			rep.add(expected, ProblemMalformed, "unexpected key %s", o.Key)
			continue
		}
		if seq != expected {
			rep.add(expected, ProblemGap, "missing seq %d..%d", expected, seq-1)
			prev = nil
		}
		expected = seq + 1
		if o.DeleteMarkers > 0 {
			rep.add(seq, ProblemDeleteMarker, "%d delete marker(s)", o.DeleteMarkers)
		}
		if len(o.VersionIDs) != 1 {
			rep.add(seq, ProblemExtraVersion, "%d versions", len(o.VersionIDs))
		}
		if len(o.VersionIDs) == 0 {
			prev = nil
			continue
		}
		body, err := sink.Get(ctx, o.Key, o.VersionIDs[0])
		if err != nil {
			return rep, fmt.Errorf("audit: read %s: %w", o.Key, err)
		}
		var e Entry
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			rep.add(seq, ProblemMalformed, "%v", err)
			prev = nil
			continue
		}
		// Verify the body is the canonical encoding of the entry.
		canonical, _ := json.Marshal(e)
		if !bytes.Equal(body, canonical) {
			rep.add(seq, ProblemMalformed, "body is not the canonical encoding of the entry")
			prev = nil
			continue
		}
		if e.Seq != seq {
			rep.add(seq, ProblemSeqMismatch, "key says %d, entry says %d", seq, e.Seq)
		}
		if e.ChainID != chainID {
			rep.add(seq, ProblemChainMismatch, "entry chain %q", e.ChainID)
		}
		switch err := CheckSeal(pub, e); {
		case errors.Is(err, ErrBadHash):
			rep.add(seq, ProblemBadHash, "hash does not match contents")
		case errors.Is(err, ErrBadSignature):
			rep.add(seq, ProblemBadSignature, "signature invalid for this public key")
		}
		switch {
		case seq == 0 && e.PrevHash != GenesisHash:
			rep.add(seq, ProblemBrokenLink, "seq 0 must link to the genesis hash")
		case prev != nil && e.PrevHash != prev.Hash:
			rep.add(seq, ProblemBrokenLink, "prevHash does not match seq %d", prev.Seq)
		}
		if e.Type == TypeCheckpoint {
			var cp CheckpointPayload
			if err := json.Unmarshal(e.Payload, &cp); err != nil {
				rep.add(seq, ProblemBadCheckpoint, "payload: %v", err)
			} else if prev != nil && (cp.PrevSeq != prev.Seq || cp.PrevHash != prev.Hash) {
				rep.add(seq, ProblemBadCheckpoint, "payload does not match seq %d", prev.Seq)
			}
		}
		hashes[seq], types[seq] = e.Hash, e.Type
		entry := e
		if rep.First == nil {
			rep.First = &entry
		}
		rep.Head, prev = &entry, &entry
		rep.Entries++
	}

	if err := verifyCheckpointIndex(ctx, sink, chainID, opts.FromSeq, hashes, types, &rep); err != nil {
		return rep, err
	}

	if want := opts.ExpectHeadAtLeast; want != nil && (rep.Head == nil || rep.Head.Seq < *want) {
		head := "none"
		if rep.Head != nil {
			head = fmt.Sprint(rep.Head.Seq)
		}
		rep.add(*want, ProblemTruncated, "chain ends at seq %s, expected at least %d", head, *want)
	}
	slices.SortStableFunc(rep.Problems, func(a, b Problem) int { return cmp.Compare(a.Seq, b.Seq) })
	return rep, nil
}

func verifyCheckpointIndex(ctx context.Context, sink Sink, chainID string, fromSeq uint64, hashes, types map[uint64]string, rep *Report) error {
	idx, err := sink.List(ctx, CheckpointPrefix(chainID), "")
	if err != nil {
		return fmt.Errorf("audit: list checkpoints: %w", err)
	}
	for _, o := range idx {
		seq, err := SeqFromKey(o.Key)
		if err != nil {
			rep.add(0, ProblemBadCheckpoint, "unexpected key %s", o.Key)
			continue
		}
		if seq < fromSeq {
			continue
		}
		if len(o.VersionIDs) != 1 {
			rep.add(seq, ProblemBadCheckpoint, "index has %d versions", len(o.VersionIDs))
			continue
		}
		body, err := sink.Get(ctx, o.Key, o.VersionIDs[0])
		if err != nil {
			return fmt.Errorf("audit: read %s: %w", o.Key, err)
		}
		var ci CheckpointIndex
		if err := json.Unmarshal(body, &ci); err != nil {
			rep.add(seq, ProblemBadCheckpoint, "index: %v", err)
			continue
		}
		if ci.Seq != seq || hashes[seq] == "" || ci.Hash != hashes[seq] || types[seq] != TypeCheckpoint {
			rep.add(seq, ProblemBadCheckpoint, "index does not match a checkpoint entry in the chain")
		}
	}
	return nil
}
