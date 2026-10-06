package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	_, _ = env.callLLM(&gatewayv1.Message{Role: "assistant", Content: "a"}, &gatewayv1.Message{Role: roleUser, Content: "second"})
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
