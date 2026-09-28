package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

var (
	ErrWriterBroken     = errors.New("audit: writer is broken; restart the gateway to resynchronize")
	ErrChainConflict    = errors.New("audit: chain conflict: a different entry already exists at this seq")
	ErrAuditUnavailable = errors.New("audit: entry could not be recorded; action refused")
)

// WriterConfig configures a Writer. Zero values get defaults except Retention, which is required.
type WriterConfig struct {
	ChainID            string
	Retention          time.Duration // required; must be positive
	CheckpointEvery    uint64
	CheckpointInterval time.Duration
	PutAttempts        int
	RetryBackoff       time.Duration
	PutTimeout         time.Duration // default 10s
	Now                func() time.Time
	Actor              string
}

func (c WriterConfig) withDefaults() WriterConfig {
	if c.CheckpointEvery == 0 {
		c.CheckpointEvery = 1000
	}
	if c.CheckpointInterval == 0 {
		c.CheckpointInterval = 10 * time.Minute
	}
	if c.PutAttempts == 0 {
		c.PutAttempts = 3
	}
	if c.RetryBackoff == 0 {
		c.RetryBackoff = 200 * time.Millisecond
	}
	if c.PutTimeout == 0 {
		c.PutTimeout = 10 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Actor == "" {
		c.Actor = "gateway"
	}
	return c
}

// Writer appends sealed entries to one chain. It is safe for concurrent use.
// Once broken it refuses every write until a new Writer is opened.
type Writer struct {
	cfg    WriterConfig
	sink   Sink
	signer *Signer

	mu              sync.Mutex
	nextSeq         uint64
	prevHash        string
	sinceCheckpoint uint64
	lastCheckpoint  time.Time
	broken          bool
	onWrite         func(Entry)
	onFailure       func(error)
}

// Open verifies the chain from its latest checkpoint, resumes after its head, and
// records a gateway.start entry. It refuses a chain that fails verification.
func Open(ctx context.Context, sink Sink, signer *Signer, cfg WriterConfig, startPayload any) (*Writer, error) {
	if cfg.Retention <= 0 {
		return nil, errors.New("audit: WriterConfig.Retention must be positive")
	}
	cfg = cfg.withDefaults()
	from, err := latestCheckpointSeq(ctx, sink, cfg.ChainID)
	if err != nil {
		return nil, err
	}
	rep, err := Verify(ctx, sink, signer.Public(), cfg.ChainID, VerifyOptions{FromSeq: from})
	if err != nil {
		return nil, err
	}
	if p, bad := rep.FirstBad(); bad {
		return nil, fmt.Errorf("audit: chain failed verification at seq %d (%s): %s", p.Seq, p.Kind, p.Detail)
	}
	w := &Writer{cfg: cfg, sink: sink, signer: signer, prevHash: GenesisHash, lastCheckpoint: cfg.Now()}
	if rep.Head != nil {
		w.nextSeq, w.prevHash = rep.Head.Seq+1, rep.Head.Hash
	}
	if _, err := w.Append(ctx, Draft{Type: TypeGatewayStart, Actor: cfg.Actor, Payload: startPayload}); err != nil {
		return nil, err
	}
	return w, nil
}

func latestCheckpointSeq(ctx context.Context, sink Sink, chainID string) (uint64, error) {
	idx, err := sink.List(ctx, CheckpointPrefix(chainID), "")
	if err != nil {
		return 0, fmt.Errorf("audit: list checkpoints: %w", err)
	}
	if len(idx) == 0 {
		return 0, nil
	}
	return SeqFromKey(idx[len(idx)-1].Key)
}

func (w *Writer) SetOnWrite(f func(Entry))   { w.mu.Lock(); defer w.mu.Unlock(); w.onWrite = f }
func (w *Writer) SetOnFailure(f func(error)) { w.mu.Lock(); defer w.mu.Unlock(); w.onFailure = f }

func (w *Writer) Broken() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.broken }

// Head returns the last written entry's seq and hash.
func (w *Writer) Head() (uint64, string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.nextSeq == 0 {
		return 0, "", false
	}
	return w.nextSeq - 1, w.prevHash, true
}

// Append seals d as the next entry and stores it. A checkpoint may follow it;
// if that checkpoint fails, the error is returned even though d was stored.
func (w *Writer) Append(ctx context.Context, d Draft) (Entry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, err := w.appendLocked(ctx, d)
	if err != nil {
		return Entry{}, err
	}
	return e, w.checkpointLocked(ctx, false)
}

// CheckpointIfDue writes a checkpoint if CheckpointInterval has passed since the
// last one and something was written since. Call it periodically.
func (w *Writer) CheckpointIfDue(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checkpointLocked(ctx, true)
}

func (w *Writer) appendLocked(ctx context.Context, d Draft) (Entry, error) {
	if w.broken {
		return Entry{}, ErrWriterBroken
	}
	payload, err := marshalPayload(d.Payload)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{
		V: EntryVersion, Seq: w.nextSeq, ChainID: w.cfg.ChainID, PrevHash: w.prevHash,
		TS:   w.cfg.Now().UTC().Format(TimeFormat),
		Type: d.Type, Actor: d.Actor, Intent: d.Intent, Stage: d.Stage,
		SubjectHash: d.SubjectHash, AuthorizedBy: d.AuthorizedBy, Payload: payload,
	}
	if err := w.signer.Seal(&e); err != nil {
		return Entry{}, err
	}
	body, err := Canonical(e)
	if err != nil {
		return Entry{}, err
	}
	if err := w.putLocked(ctx, EntryKey(w.cfg.ChainID, e.Seq), body); err != nil {
		return Entry{}, err
	}
	w.nextSeq++
	w.prevHash = e.Hash
	if e.Type != TypeCheckpoint {
		w.sinceCheckpoint++
	}
	if w.onWrite != nil {
		w.onWrite(e)
	}
	return e, nil
}

func (w *Writer) checkpointLocked(ctx context.Context, byTime bool) error {
	due := w.sinceCheckpoint >= w.cfg.CheckpointEvery ||
		(byTime && w.sinceCheckpoint > 0 && w.cfg.Now().Sub(w.lastCheckpoint) >= w.cfg.CheckpointInterval)
	if !due {
		return nil
	}
	cp, err := w.appendLocked(ctx, Draft{
		Type: TypeCheckpoint, Actor: w.cfg.Actor,
		Payload: CheckpointPayload{PrevSeq: w.nextSeq - 1, PrevHash: w.prevHash},
	})
	if err != nil {
		return err
	}
	idx, err := json.Marshal(CheckpointIndex{Seq: cp.Seq, Hash: cp.Hash})
	if err != nil {
		return err
	}
	if err := w.putLocked(ctx, CheckpointKey(w.cfg.ChainID, cp.Seq), idx); err != nil {
		return err
	}
	w.sinceCheckpoint = 0
	w.lastCheckpoint = w.cfg.Now()
	return nil
}

// putLocked writes body, retrying the identical bytes. An ErrExists whose stored
// bytes match is an earlier attempt that landed; anything else breaks the writer.
func (w *Writer) putLocked(ctx context.Context, key string, body []byte) error {
	retainUntil := w.cfg.Now().Add(w.cfg.Retention)
	detached := context.WithoutCancel(ctx)
	var lastErr error
	for attempt := 0; attempt < w.cfg.PutAttempts; attempt++ {
		if attempt > 0 {
			<-time.After(w.cfg.RetryBackoff)
		}
		putCtx, cancel := context.WithTimeout(detached, w.cfg.PutTimeout)
		err := w.sink.Put(putCtx, key, body, retainUntil)
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrExists) {
			getCtx, cancel := context.WithTimeout(detached, w.cfg.PutTimeout)
			existing, getErr := w.sink.Get(getCtx, key, "")
			cancel()
			if getErr == nil && bytes.Equal(existing, body) {
				return nil
			}
			return w.fail(fmt.Errorf("%w (key %s)", ErrChainConflict, key))
		}
		lastErr = err
	}
	return w.fail(fmt.Errorf("%w: %v", ErrWriterBroken, lastErr))
}

func (w *Writer) fail(err error) error {
	w.broken = true
	if w.onFailure != nil {
		w.onFailure(err)
	}
	return err
}

// maxExactJSONInt is 2^53: every integer with a magnitude up to this is exactly
// representable as an IEEE 754 double, so RFC 8785 serializes it unchanged.
var maxExactJSONInt = new(big.Int).Lsh(big.NewInt(1), 53)

// marshalPayload encodes a payload and rejects any JSON number RFC 8785 would
// alter when the entry is canonicalized for hashing and storage: the signed
// bytes must mean exactly what the caller wrote (spec §14.2).
func marshalPayload(p any) (json.RawMessage, error) {
	var b []byte
	switch v := p.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		b = v
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			return nil, fmt.Errorf("audit: marshal payload: %w", err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("audit: payload is not valid JSON: %w", err)
	}
	if err := checkExactNumbers(doc); err != nil {
		return nil, err
	}
	return b, nil
}

// checkExactNumbers walks a decoded JSON value and rejects numbers that are
// not integer literals or whose magnitude exceeds 2^53.
func checkExactNumbers(v any) error {
	switch v := v.(type) {
	case map[string]any:
		for _, x := range v {
			if err := checkExactNumbers(x); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range v {
			if err := checkExactNumbers(x); err != nil {
				return err
			}
		}
	case json.Number:
		n, ok := new(big.Int).SetString(string(v), 10)
		if !ok || n.CmpAbs(maxExactJSONInt) > 0 {
			return fmt.Errorf("audit: payload number %s is non-integral or exceeds 2^53 in magnitude, so RFC 8785 canonical JSON would alter it; encode it as a string", v)
		}
	}
	return nil
}
