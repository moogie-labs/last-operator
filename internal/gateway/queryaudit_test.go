package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func (e *testEnv) query(req *gatewayv1.QueryAuditRequest) (*gatewayv1.QueryAuditResponse, error) {
	if req.Intent == nil {
		req.Intent = ref()
	}
	return e.g.queryAudit(e.ctx, req)
}

// twoLLMCalls makes two CallLLM calls for the env's Intent: four indexed entries.
func twoLLMCalls(t *testing.T, env *testEnv) {
	t.Helper()
	if _, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "first"}); err != nil {
		t.Fatal(err)
	}
	// The script has one answer, so this call fails, but it still records request and response.
	_, _ = env.callLLM(&gatewayv1.Message{Role: roleAssistant, Content: "a"}, &gatewayv1.Message{Role: roleUser, Content: "second"})
}

func TestQueryAuditReturnsTheIntentsEntriesOldestFirst(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	resp, err := env.query(&gatewayv1.QueryAuditRequest{Types: []string{audit.TypeLLMRequest}})
	if err != nil || len(resp.GetEntries()) != 2 || resp.GetTruncated() || resp.GetMore() {
		t.Fatalf("QueryAudit = %v, %v", resp, err)
	}
	first, second := resp.GetEntries()[0], resp.GetEntries()[1]
	if first.GetSeq() >= second.GetSeq() || first.GetType() != audit.TypeLLMRequest {
		t.Fatalf("entries = %v", resp.GetEntries())
	}
	var p llmRequestPayload
	if err := json.Unmarshal(second.GetPayload(), &p); err != nil || len(p.Messages) != 2 || p.Messages[1].Content != "second" {
		t.Fatalf("payload = %s, %v", second.GetPayload(), err)
	}
	after, err := env.query(&gatewayv1.QueryAuditRequest{Types: []string{audit.TypeLLMRequest}, AfterSeq: first.GetSeq()})
	if err != nil || len(after.GetEntries()) != 1 || after.GetEntries()[0].GetSeq() != second.GetSeq() {
		t.Fatalf("after_seq = %v, %v", after, err)
	}
	all, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || len(all.GetEntries()) != 4 {
		t.Fatalf("every type = %v, %v", all, err)
	}
}

func TestQueryAuditNeverReturnsOtherIntentsEntries(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	api := intentWithPlan(t, twoSteps())
	api.Name, api.UID = "api", "api-uid"
	if err := env.crd.Create(env.ctx, api); err != nil {
		t.Fatal(err)
	}
	cm, err := env.kube.CoreV1().ConfigMaps(ns).Get(context.Background(), "llm-script", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data["team-x.api"] = `["api answer"]`
	if _, err := env.kube.CoreV1().ConfigMaps(ns).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	apiRef := &gatewayv1.IntentRef{Namespace: envNS, Name: "api"}
	if _, err := env.g.callLLM(env.ctx, &gatewayv1.CallLLMRequest{Intent: apiRef, Messages: []*gatewayv1.Message{{Role: roleUser, Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	twoLLMCalls(t, env)
	owner := map[uint64]string{}
	for _, e := range env.entries() {
		owner[e.Seq] = e.Intent
	}
	for intent, r := range map[string]*gatewayv1.IntentRef{envKey: ref(), envNS + "/api": apiRef} {
		resp, err := env.query(&gatewayv1.QueryAuditRequest{Intent: r})
		if err != nil || len(resp.GetEntries()) == 0 {
			t.Fatalf("%s: QueryAudit = %v, %v", intent, resp, err)
		}
		for _, e := range resp.GetEntries() {
			if owner[e.GetSeq()] != intent {
				t.Fatalf("%s: returned entry %d of %s", intent, e.GetSeq(), owner[e.GetSeq()])
			}
		}
	}
}

func TestQueryAuditAfterSeqBeyondTheHeadIsEmpty(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	resp, err := env.query(&gatewayv1.QueryAuditRequest{AfterSeq: 1 << 40})
	if err != nil || len(resp.GetEntries()) != 0 || resp.GetMore() {
		t.Fatalf("QueryAudit = %v, %v", resp, err)
	}
}

func TestQueryAuditLimitPagesWithMore(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	page, err := env.query(&gatewayv1.QueryAuditRequest{Limit: 1})
	if err != nil || len(page.GetEntries()) != 1 || !page.GetMore() {
		t.Fatalf("first page = %v, %v", page, err)
	}
	next, err := env.query(&gatewayv1.QueryAuditRequest{Limit: 3, AfterSeq: page.GetEntries()[0].GetSeq()})
	if err != nil || len(next.GetEntries()) != 3 || next.GetMore() {
		t.Fatalf("next page = %v, %v", next, err)
	}
}

func TestQueryAuditRejectsBadQueries(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	for name, req := range map[string]*gatewayv1.QueryAuditRequest{
		"checkpoint type": {Types: []string{audit.TypeCheckpoint}},
		"policy.denied":   {Types: []string{audit.TypePolicyDenied}},
		"limit too large": {Limit: maxIndexEntries + 1},
		"negative limit":  {Limit: -1},
		"bad intent":      {Intent: &gatewayv1.IntentRef{Namespace: "Not_A_Namespace", Name: "x"}},
	} {
		if _, err := env.query(req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	env.g.writer.Store(nil)
	if _, err := env.query(&gatewayv1.QueryAuditRequest{}); status.Code(err) != codes.Unavailable {
		t.Errorf("no writer: %v, want Unavailable", err)
	}
}

func TestQueryAuditTheIndexSurvivesARestart(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	before, err := env.query(&gatewayv1.QueryAuditRequest{}) // flushes the index
	if err != nil {
		t.Fatal(err)
	}
	env.g = env.newGateway()
	after, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || len(after.GetEntries()) != len(before.GetEntries()) {
		t.Fatalf("after a restart: %v, %v; want %d entries", after, err, len(before.GetEntries()))
	}
	// Entries collected but not flushed before a restart are missing, never an error.
	_, _ = env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "lost"})
	env.g = env.newGateway()
	again, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || len(again.GetEntries()) != len(before.GetEntries()) {
		t.Fatalf("unflushed entries: %v, %v", again, err)
	}
}

func TestQueryAuditReportsTruncation(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	if _, err := env.query(&gatewayv1.QueryAuditRequest{}); err != nil {
		t.Fatal(err)
	}
	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	rec.DropIndex(1)
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	resp, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || !resp.GetTruncated() || len(resp.GetEntries()) != 3 {
		t.Fatalf("after_seq 0 = %v, %v; want truncated", resp, err)
	}
	resp, err = env.query(&gatewayv1.QueryAuditRequest{AfterSeq: rec.IndexFloor})
	if err != nil || resp.GetTruncated() {
		t.Fatalf("after the floor = %v, %v; want not truncated", resp, err)
	}
}

func TestQueryAuditRefusesAnEntryThatIsNotWhatTheIndexSays(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	if _, err := env.query(&gatewayv1.QueryAuditRequest{}); err != nil {
		t.Fatal(err)
	}
	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	rec.Index[0].Type = audit.TypeCommandResult
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := env.query(&gatewayv1.QueryAuditRequest{}); status.Code(err) != codes.Internal {
		t.Fatalf("QueryAudit = %v, want Internal", err)
	}
}

func TestFlushIndexDropsOldestEntriesWhenTheRecordIsFull(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	rec.Executed = &ExecutedPlan{Stage: "Create", PlanHash: "sha256:x", Plan: strings.Repeat("p", MaxRecordBytes-2000)}
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= maxIndexEntries; seq++ {
		env.g.index.observe(audit.Entry{Seq: seq, Type: audit.TypeLLMRequest, Intent: envKey})
	}
	if err := env.g.flushIndex(env.ctx, envKey); err != nil {
		t.Fatal(err)
	}
	rec, err = env.g.state.Load(env.ctx, envKey)
	if err != nil || len(rec.Index) == 0 || len(rec.Index) == maxIndexEntries || rec.IndexFloor == 0 || rec.Executed == nil {
		t.Fatalf("record = %d index entries, floor %d, executed %v; %v", len(rec.Index), rec.IndexFloor, rec.Executed != nil, err)
	}
}

func TestSaveRecordDropsIndexEntriesSoPhaseStateAlwaysFits(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= maxIndexEntries; seq++ {
		rec.Index = append(rec.Index, IndexEntry{Seq: seq, Type: audit.TypeLLMRequest})
	}
	rec.Executed = &ExecutedPlan{Stage: "Create", PlanHash: "sha256:x", Plan: strings.Repeat("p", MaxRecordBytes-3000)}
	if err := env.g.state.Save(env.ctx, rec); !errors.Is(err, errRecordTooLarge) {
		t.Fatalf("setup: Save = %v, want errRecordTooLarge", err)
	}
	if err := env.g.saveRecord(env.ctx, rec); err != nil {
		t.Fatalf("saveRecord = %v", err)
	}
	got, err := env.g.state.Load(env.ctx, envKey)
	if err != nil || got.Executed == nil || len(got.Index) >= maxIndexEntries || got.IndexFloor == 0 {
		t.Fatalf("record = %d index entries, floor %d, executed %v; %v", len(got.Index), got.IndexFloor, got.Executed != nil, err)
	}
}

// failRecordSaves makes the fake cluster refuse to create or update Intent
// record ConfigMaps while fail is set; other ConfigMaps are unaffected.
func failRecordSaves(env *testEnv, fail *atomic.Bool) {
	env.kube.PrependReactor("*", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		write, ok := a.(interface{ GetObject() runtime.Object }) // create and update
		if !ok || !fail.Load() {
			return false, nil, nil
		}
		if cm, ok := write.GetObject().(*corev1.ConfigMap); ok && strings.HasPrefix(cm.Name, "last-intent-") {
			return true, nil, errors.New("injected: the API server is unavailable")
		}
		return false, nil, nil
	})
}

func TestQueryAuditIsUnavailableWhenItCannotSaveTheIndex(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	var fail atomic.Bool
	failRecordSaves(env, &fail)
	if _, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}

	// No record yet: the pre-read flush would create it.
	fail.Store(true)
	if resp, err := env.query(&gatewayv1.QueryAuditRequest{}); status.Code(err) != codes.Unavailable || resp != nil {
		t.Fatalf("create fails: QueryAudit = %v, %v; want Unavailable", resp, err)
	}
	if n := len(env.g.index.pending[envKey]); n != 2 {
		t.Fatalf("after a failed create: %d pending entries, want 2 restored", n)
	}
	fail.Store(false)
	first, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || len(first.GetEntries()) != 2 {
		t.Fatalf("after recovery: QueryAudit = %v, %v; want 2 entries", first, err)
	}

	// The record exists now: the pre-read flush would update it.
	_, _ = env.callLLM(&gatewayv1.Message{Role: roleAssistant, Content: "a"}, &gatewayv1.Message{Role: roleUser, Content: "again"})
	fail.Store(true)
	if resp, err := env.query(&gatewayv1.QueryAuditRequest{}); status.Code(err) != codes.Unavailable || resp != nil {
		t.Fatalf("update fails: QueryAudit = %v, %v; want Unavailable", resp, err)
	}
	if n := len(env.g.index.pending[envKey]); n != 2 {
		t.Fatalf("after a failed update: %d pending entries, want 2 restored", n)
	}
	fail.Store(false)
	all, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || len(all.GetEntries()) != 4 || all.GetTruncated() {
		t.Fatalf("after recovery: QueryAudit = %v, %v; want all 4 entries", all, err)
	}
}

func TestPeriodicFlushSkipsABusyIntentAndKeepsItsEntriesPending(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	if _, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	unlock := env.g.locks.lock(envKey) // as EndPhase holds it while a step runs
	done := make(chan struct{})
	go func() { env.g.flushIndexes(env.ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		unlock()
		t.Fatal("the periodic flush blocked on a busy Intent")
	}
	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil || len(rec.Index) != 0 {
		t.Fatalf("busy Intent: record index = %v, %v; want nothing saved", rec.Index, err)
	}
	if n := len(env.g.index.pending[envKey]); n != 2 {
		t.Fatalf("busy Intent: %d pending entries, want 2", n)
	}
	unlock()
	env.g.flushIndexes(env.ctx)
	rec, err = env.g.state.Load(env.ctx, envKey)
	if err != nil || len(rec.Index) != 2 || len(env.g.index.pending[envKey]) != 0 {
		t.Fatalf("idle Intent: record index = %v, %v; want both entries saved", rec.Index, err)
	}
}

func TestQueryAuditRefusesATamperedEntry(t *testing.T) {
	cases := map[string]struct {
		change func(t *testing.T, e *audit.Entry)
		want   error
	}{
		"body": {func(_ *testing.T, e *audit.Entry) {
			e.Payload = json.RawMessage(`{"messages":[{"role":"user","content":"forged"}]}`)
		}, audit.ErrBadHash},
		"signature": {func(t *testing.T, e *audit.Entry) {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			if err := audit.NewSigner(other).Seal(e); err != nil {
				t.Fatal(err)
			}
		}, audit.ErrBadSignature},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newLLMEnv(t, lastv1alpha1.LLMScripted)
			twoLLMCalls(t, env)
			resp, err := env.query(&gatewayv1.QueryAuditRequest{Types: []string{audit.TypeLLMRequest}})
			if err != nil || len(resp.GetEntries()) != 2 {
				t.Fatalf("before tampering: QueryAudit = %v, %v", resp, err)
			}
			key := audit.EntryKey(testChainID, resp.GetEntries()[1].GetSeq())
			body, err := env.sink.Get(env.ctx, key, "")
			if err != nil {
				t.Fatal(err)
			}
			e, err := audit.DecodeEntry(body)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(t, &e)
			forged, err := audit.Canonical(e)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := audit.DecodeEntry(forged); err != nil {
				t.Fatalf("setup: the forged entry must still decode strictly: %v", err)
			}
			env.sink.TamperReplace(key, forged)
			_, err = env.query(&gatewayv1.QueryAuditRequest{})
			if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), tc.want.Error()) {
				t.Fatalf("QueryAudit = %v, want Internal for %v", err, tc.want)
			}
		})
	}
}
