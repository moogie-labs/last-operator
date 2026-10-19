package gateway

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

const (
	defaultQueryLimit = 50
	// maxQueryBytes bounds a response's payloads below the 4 MiB default gRPC
	// receive size; the caller pages with after_seq when more is set.
	maxQueryBytes = 3 << 20
)

// queryAudit returns one Intent's indexed audit entries, oldest first, each read
// from the sink and verified (M3 spec §3.5). It writes no audit entry.
func (g *Gateway) queryAudit(ctx context.Context, req *gatewayv1.QueryAuditRequest) (*gatewayv1.QueryAuditResponse, error) {
	_, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, t := range req.GetTypes() {
		if !indexedTypes[t] {
			return nil, status.Errorf(codes.InvalidArgument, "type %q is not served by QueryAudit", truncateReason(t))
		}
		want[t] = true
	}
	limit := int(req.GetLimit())
	switch {
	case limit == 0:
		limit = defaultQueryLimit
	case limit < 0 || limit > maxIndexEntries:
		return nil, status.Errorf(codes.InvalidArgument, "limit must be between 1 and %d", maxIndexEntries)
	}
	// Read-your-writes: entries the writer collected must be saved before the
	// read, or the caller would see a stale index that does not report the gap.
	// flushIndex restores the entries it took, so a later query serves them.
	if err := g.flushIndex(ctx, key); err != nil {
		return nil, status.Errorf(codes.Unavailable, "could not save the audit index: %v", err)
	}
	var in lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &in); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	rec, err := g.state.Load(ctx, key, string(in.UID))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	g.metrics.ObserveQuery()
	resp := &gatewayv1.QueryAuditResponse{Truncated: req.GetAfterSeq() < rec.IndexFloor}
	size := 0
	for _, ie := range rec.Index {
		if ie.Seq <= req.GetAfterSeq() || (len(want) > 0 && !want[ie.Type]) {
			continue
		}
		if len(resp.Entries) == limit {
			resp.More = true
			break
		}
		e, err := g.readIndexed(ctx, key, ie)
		if err != nil {
			return nil, err
		}
		if len(resp.Entries) > 0 && size+len(e.Payload) > maxQueryBytes {
			resp.More = true
			break
		}
		size += len(e.Payload)
		resp.Entries = append(resp.Entries, &gatewayv1.AuditEntry{Seq: e.Seq, Type: e.Type, Ts: e.TS, Stage: e.Stage,
			SubjectHash: e.SubjectHash, Payload: e.Payload})
	}
	return resp, nil
}

// readIndexed reads one indexed entry from the sink and verifies it: strict
// decoding, the signature, and that it is the entry the index says it is.
func (g *Gateway) readIndexed(ctx context.Context, intent string, ie IndexEntry) (audit.Entry, error) {
	body, err := g.sink.Get(ctx, audit.EntryKey(g.cfg.ChainID, ie.Seq), "")
	if errors.Is(err, audit.ErrNotFound) {
		return audit.Entry{}, status.Errorf(codes.Internal, "indexed audit entry %d does not exist", ie.Seq)
	}
	if err != nil {
		return audit.Entry{}, status.Errorf(codes.Unavailable, "read audit entry %d: %v", ie.Seq, err)
	}
	e, err := audit.DecodeEntry(body)
	if err != nil {
		return audit.Entry{}, status.Errorf(codes.Internal, "audit entry %d failed verification: %v", ie.Seq, err)
	}
	if err := audit.CheckSeal(g.signer.Public(), e); err != nil {
		return audit.Entry{}, status.Errorf(codes.Internal, "audit entry %d failed verification: %v", ie.Seq, err)
	}
	if e.Seq != ie.Seq || e.Intent != intent || e.Type != ie.Type {
		return audit.Entry{}, status.Errorf(codes.Internal, "audit entry %d is not the indexed %s of %s", ie.Seq, ie.Type, intent)
	}
	return e, nil
}
