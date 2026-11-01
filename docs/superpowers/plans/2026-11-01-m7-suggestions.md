# M7: Rule Suggestions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The operator proposes allow rules (CommandRuleSuggestions) at plan time, on repeated approvals and on a periodic review. Only humans accept or deny them, and a denied proposal, or a broader one, is never suggested again.

**Architecture:**
- **`internal/rules`** gains pattern containment, rule subsumption, group keys and a deterministic merge.
- **A new `internal/suggest` package, with no Kubernetes client,** holds the vetting gate, naming, the LLM prompt, schema and parser, the comparison of a proposal with `Generalize`, and display sanitizing. The operator and the CLI share it.
- **The gateway** tags LLM calls with a purpose, exposes `authorizedBy` in QueryAudit, and audits decisions.
- **The operator** runs the three triggers.
- **The CLI** shows, accepts and denies suggestions, and never trusts operator text (design §2).

**Tech Stack:** Go 1.27, controller-runtime, kubebuilder markers, buf/protobuf, Ginkgo envtest, Helm, ValidatingAdmissionPolicy, kind e2e.

**Spec:** `docs/superpowers/specs/2026-11-01-m7-suggestions-design.md` (§2 trust model, S1–S10, §4 triggers, §5 deciding, §6 audit, §8 errors, §10 testing, §11 amendments, §12 upgrading). Main spec: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §4.1, §5.5, §5.6, §9.4, §9.5.

## Global Constraints

- **Commits and files:**
  - never add a commit attribution trailer (such as `Co-authored-by:`); never commit `.superpowers/`;
  - never edit generated files; after changing `api/`, run `make manifests generate helm-crds`; after changing `proto/`, run `make proto`;
  - after Go changes, run `make lint-fix`, `make lint` (0 issues) and `make test`; after chart changes, run `make helm-lint`;
  - credential values never go in audit payloads, logs or errors.
- **Trust (design §2):**
  - no operator-written text is used by an automated step;
  - `approve --always-allow=exact|generalized` never uses suggestions;
  - there is no "accept all";
  - the CLI recomputes lint, matches and breadth itself, ignores `spec.breadthWarnings`, strips control characters from operator text before printing it, and refuses to accept a proposal with a `Nothing` or `Code` lint finding.
- **Names and limits:**
  - suggestion name `sugg-<hash10>`: the first 10 hex characters of the SHA-256 of the canonical JSON of `{targetScope, namespace, proposed}`;
  - at most **100** Pending suggestions per namespace, with the event `SuggestionLimit`;
  - label `last.moogie-labs.com/intent`;
  - Cluster-target suggestions live in `last-system`;
  - `reviewInterval` defaults to `24h`, from `1h` to `720h`, with ±10% jitter;
  - `repeatThreshold` defaults to `3`, with minimum `1`.
- **Events** (on the Intent, or on the suggestion's namespace's newest example Intent): `SuggestionFailed`, `SuggestionDropped`, `SuggestionSuppressed`, `SuggestionLimit`, `SuggestionCreated`.
- **The LLM** (S10):
  - `CallLLMRequest.purpose`: `""`/`"turn"` (default) or `"suggest"`;
  - the suggest schema is `{suggestions: [{entry, argv, rationale}]}`, with no regex keywords;
  - the scripted key is `_suggest.<namespace>.<intent>`; a missing key means no proposal (no error event);
  - a failed suggest call makes no suggestion and is not retried;
  - suggest calls are audited, record `"purpose": "suggest"` in the `llm.request` and `llm.response` payloads, and are not added to the Intent's audit index.
- **Stages** (§4.1):
  - steps and probes of a Create or Update plan: `[plan stage, Remediate]`;
  - steps and probes of other plans: `[plan stage]`;
  - teardown: `[Delete]`.
  - Create and Update are one class everywhere (M6 R15).
- **Authority:** only humans decide; `decidedBy == request.userInfo.username` whenever the decision is set; the operator only creates Pending suggestions and never updates or deletes them.
- **Rulings made while writing this plan:**
  - the gateway observes decisions by listing suggestions every 30s from its leader loop, through its uncached client. The design says "informer"; the gateway has no informers, and a poll is equivalent for "observed once";
  - the RepeatedApproval scan of a namespace is queued after any successful EndPhase in it; the scan itself keeps only `authorizedBy.kind == "approval"`;
  - the operator gets a `--system-namespace` flag, default `last-system`.

## Review Focus

1. **Two people accept the same suggestion at once.** The second accept finds the identical rule, fails the decision update with a resourceVersion conflict, re-reads the suggestion, sees it Accepted, and exits 0 saying who accepted it. Task 8 tests it.
2. **A person without RBAC to create rules accepts a suggestion.** The accept fails before the decision changes; the suggestion stays Pending. Task 8 tests it.
3. **Operator text with terminal escapes, or very long text.**
   - `Vet` drops a rationale over 500 characters.
   - The CLI strips control characters (C0 except `\n` and `\t`, DEL, C1, and the ESC sequences they start) from the rationale, the examples and the pattern before printing them.
   - Tasks 2 and 8 test it.
4. **The Intent is deleted while its PlanTime LLM call runs.** No error loop: a NotFound on the status patch ends the step. Any suggestion already created stays. Task 6 tests it.
5. **A proposal identical to a Denied suggestion, from another trigger.** It is reported as `SuggestionSuppressed`, never silently treated as "already exists". The suppression check runs before the create. Task 6 tests it.

---

### Task 1: Containment, subsumption, groups and merge in `internal/rules`

**Files:**
- Create: `internal/rules/contain.go`, `internal/rules/group.go`, plus `_test.go` files.
- Modify: `internal/rules/lint.go`: `Finding.Kind` replaces `Finding.Nothing`. Update every caller (`MatchesNothing`, the gateway log in `internal/gateway`, `internal/cli/rules.go`).

**Interfaces:**
- Produces:
  - `type FindingKind string`, with the constants `KindNothing`, `KindCode`, `KindDestructive`, `KindBreadth`; `Finding{Message string; Kind FindingKind}`;
  - `type Verdict int`, with the constants `No`, `Yes`, `Unknown`;
  - `func Contains(broad, narrow []string) Verdict` (S9);
  - `func Subsumes(n, d lastv1alpha1.CommandRuleSpec) Verdict`: Yes when n lets through everything d does (S8 dimensions); Unknown if any dimension is Unknown and none is No;
  - `func GroupKey(stage lastv1alpha1.Stage, cmd lastv1alpha1.Command) (string, bool)`: false for scripts and `${steps.` argv (§4.2);
  - `func Merge(argvs [][]string) ([]string, bool)` (§4.2).

- [ ] **Step 1: Write the failing tests.**
  - `TestLintKinds`: each existing lint row gets its kind. The interpreter, `--` and code-flag findings are `KindCode`; destructive-verb findings are `KindDestructive`.
  - `TestContains`, a table:
    - literal in literal, both equal and different;
    - a literal in `*`, in `[!-]*`, and a `-x` literal not in `[!-]*`;
    - a glob in `*` is Yes;
    - `re:` in `*` is Yes; `*` in `re:.*` is Unknown;
    - a narrow `...` only in a broad `...`;
    - a broad `...` covers a longer narrow tail;
    - different lengths with no `...` is No;
    - the spec example: `[aws ec2 ...]` contains `[aws ec2 describe-* ...]` (Yes), and the reverse is No; `[aws ...]` contains `[aws ec2 ...]`.
  - `TestSubsumes`:
    - one row per dimension where only that dimension makes the answer No: stages, providers, egress, install;
    - Create in d with Update in n is Yes;
    - an Unknown match makes Unknown.
  - `TestGroupKey`:
    - `kubectl get pods web -n a` and `kubectl -n b get pods api` share a key when stage and providers are equal;
    - `Create` and `Update` share a key;
    - different sorted providers give different keys;
    - `ssh host uptime` is keyed by its whole argv;
    - a script returns false.
  - `TestMerge`:
    - `[kubectl get pods web]` and `[kubectl get pods api]` → `[kubectl get pods [!-]*]`;
    - different lengths → false;
    - a differing value that starts with `-` → false;
    - a difference at or after a `StartsCode` position → false.
- [ ] **Step 2: Run `go test ./internal/rules/`.** Expected: FAIL (undefined).
- [ ] **Step 3: Implement.**
  - `Contains` decides per position: a narrow position with a single literal (`literalOf`) is decided by `Match`; equal strings are Yes; broad `*` is Yes; broad `[!-]*` is Yes for a narrow position that can't start with `-`; everything else is Unknown.
  - `Subsumes` uses the policy package's egress covers relation, as `Covers` does.
- [ ] **Step 4: Run `go test ./internal/rules/ ./internal/gateway/ ./internal/cli/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(rules): pattern containment, rule subsumption, group keys and merge"`

---

### Task 2: The `internal/suggest` package

**Files:**
- Create: `internal/suggest/vet.go`, `internal/suggest/name.go`, `internal/suggest/llm.go`, `internal/suggest/display.go`, plus `_test.go` files.

**Interfaces:**
- Consumes: Task 1.
- Produces:
  - `type Proposal struct{ TargetScope lastv1alpha1.RuleScope; Namespace string; Trigger lastv1alpha1.SuggestionTrigger; Rationale string; Spec lastv1alpha1.CommandRuleSpec; Examples []lastv1alpha1.Command; Contains [][]string }`. `Contains` holds the match patterns a consolidation must contain; it is used in place of examples when non-empty.
  - `type Denied struct{ Scope lastv1alpha1.RuleScope; Namespace, Name string; Spec lastv1alpha1.CommandRuleSpec }`.
  - `func Vet(p Proposal, existing []rules.Rule, denied []Denied) (ok bool, reason string, suppressedBy string)`, implementing S5 then S8. It also drops a rationale over 500 characters. Suppression scope follows S8: a Namespace proposal checks denied Namespace suggestions in its namespace; a Cluster proposal checks all of them; Unknown counts as suppressing.
  - `func Name(p Proposal) (string, error)` (S6).
  - `const SystemPrompt string`, `func Schema() []byte`, `func Request(entries []Entry) string`, `func Parse(content string) ([]Answer, error)`, with `type Entry struct{ ID string; Stage lastv1alpha1.Stage; Argv []string }` and `type Answer struct{ Entry string; Argv []string; Rationale string }`.
  - `func Broader(p lastv1alpha1.CommandRuleSpec, g lastv1alpha1.CommandRuleSpec) []string`: one human-readable line for each place p is broader than g. Places are a pattern position (by `rules.Contains` not Yes), an extra stage, an extra provider, extra egress, or an install entry that is not exact.
  - `func Sanitize(s string) string`.

- [ ] **Step 1: Write the failing tests.**
  - `TestVetDrops`: one row per S5 reason:
    - a pattern that matches nothing;
    - code that can vary (`bash -c *`);
    - a wildcard after `delete`;
    - an `argv[0]` mismatch;
    - an example not matched;
    - `source` set;
    - a 501-character rationale.
    - Each row asserts its reason text names the cause.
  - `TestVetSkipsCoveredAndSuppressed`:
    - existing rules covering every example → not ok, with reason "already covered";
    - a Denied `[aws ec2 ...]` suppresses `[aws ec2 ...]` and `[aws ...]`, with `suppressedBy` set to its name, and does not suppress `[aws ec2 describe-* ...]`;
    - a denied Cluster suggestion does not suppress a Namespace proposal;
    - a denied Namespace suggestion in another namespace suppresses a Cluster proposal.
  - `TestVetPassesGeneralizeOutput`: a property test over generated argvs, of the shape M6's `generalize_test.go` uses. `Generalize` output with the command as its example passes the lint parts of Vet.
  - `TestNameIsStableAndScoped`: same input gives the same name; a different namespace gives a different name; the format matches `^sugg-[0-9a-f]{10}$`.
  - `TestParse`: valid JSON; extra fields ignored; non-JSON is an error; an empty list is ok.
  - `TestSchemaHasNoRegexKeywords`: the schema has no `pattern` or `format` keys and is under 1024 bytes.
  - `TestBroader`: a proposal equal to g gives no lines; an extra `Remediate` stage, an extra provider, and `[!-]*` where g has a literal each give one line.
  - `TestSanitize`: `"a\x1b[31mb\x07c\u009bd"` → `"abcd"`; `\n` and `\t` are kept.
- [ ] **Step 2: Run `go test ./internal/suggest/`.** Expected: FAIL.
- [ ] **Step 3: Implement.** The system prompt states:
  - the pattern syntax (main spec §9.2);
  - that `argv[0]` must stay literal;
  - that code, a command runner's command, and anything after `--` must stay literal;
  - no wildcards after destructive verbs;
  - one pattern per entry, which must match that entry's argv.
- [ ] **Step 4: Run `go test ./internal/suggest/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(suggest): vet, name, prompt and display rule suggestions"`

---

### Task 3: API, admission and RBAC

**Files:**
- Modify:
  - `api/v1alpha1/commandrulesuggestion_types.go`: S2 transition rules. Fields other than `decision`, `decidedBy` and `decisionReason` are immutable; once decided, all three are frozen. CRD CEL is the ruling: it applies to every requester and is tested in envtest.
  - `api/v1alpha1/commandrule_types.go`: `RuleSource.CreatedBy string` (`json:"createdBy,omitempty"`).
  - `api/v1alpha1/intent_types.go`: add `Plan.Suggested bool` (`json:"suggested,omitempty"`); remove `Plan.Generalizations`, `PendingCommand.Generalization` and the `Generalization` type.
  - `api/v1alpha1/operatorconfig_types.go`: `ReviewInterval` gets a CEL bound from 1h to 720h.
  - `internal/cli/rules_test.go`: remove the two tests that set the removed fields.
  - `internal/cli/rules.go`:
    - `createRules` sets `source.createdBy` to the SelfSubjectReview user;
    - `ruleName` and `sameSpec` hash and compare the spec without `source`.
  - Create `charts/last-operator/files/suggestions-policy.yaml`, template it in `admission.yaml` beside `rules-policy.yaml`, and implement S1.
  - `charts/last-operator/files/rules-policy.yaml`: S4.
  - `charts/last-operator/files/operator-clusterrole.yaml`, and the RBAC markers in `internal/controller/intent_controller.go`: `commandrulesuggestions`: get, list, watch, create.
  - `charts/last-operator/templates/gateway.yaml` (ClusterRole `last-gateway-lifecycle`): `commandrulesuggestions`: get, list, watch. A Role in the gateway namespace: get, create and update on the ConfigMap `last-suggestion-decisions`.
  - A new ClusterRole `last-suggestion-decider` in the chart: get, list, watch, update and patch on `commandrulesuggestions`.
  - Makefile `helm-lint`:
    - both policies render;
    - the operator has no update, patch or delete on suggestions;
    - the gateway has no write on suggestions.
  - Main spec §4.1, §5.5, §5.6 and §9.4: replace the M6 note on generalizations with a pointer to the M7 design §2.
- Test: `test/admission/admission_test.go` and `test/admission/operator_rbac_test.go`; CRD CEL in `api/v1alpha1/rules_validation_test.go`.

**Interfaces:**
- Produces:
  - S1–S4 in force;
  - `lastv1alpha1.RuleSource.CreatedBy`;
  - `lastv1alpha1.Plan.Suggested`;
  - `CommandRuleSpec` canonical JSON without `source`, through `func ruleSpecKey(spec) ([]byte, error)` in `internal/cli/rules.go`.

- [ ] **Step 1: Write the failing tests.**
  - `TestSuggestionWritesFollowS1`:
    - the operator SA creates a Pending suggestion: allowed;
    - the operator SA creates one with `decision: Denied`, or with `decidedBy` set, or with `proposed.source` set: refused;
    - the operator SA updates: refused;
    - a gateway-namespace SA creates: refused;
    - a Cluster target in `team-x`: refused;
    - a user sets `decision: Denied` with `decidedBy` equal to themselves: allowed;
    - a user sets it with `decidedBy` equal to another user: refused;
    - a user changes a label on a suggestion another user decided: allowed.
  - `TestSuggestionsAreImmutableApartFromTheDecision` (CEL): changing `proposed` or `rationale` is refused; changing `decisionReason` after the decision is refused.
  - `TestRuleSourceNamesItsAuthor`:
    - creating with `source.createdBy` equal to someone else: refused;
    - creating with yourself: allowed;
    - creating without `source`: allowed;
    - an update that changes `source`: refused;
    - `createdFrom: suggestion` without `suggestionRef`: refused.
  - The operator RBAC test: the operator can create suggestions and cannot update or delete them.
  - CLI: `TestAlwaysAllowStampsCreatedBy`, and `TestIdenticalRuleBySomeoneElseCountsAsDone`.
- [ ] **Step 2: Run `go test ./test/admission/ ./internal/cli/ ./api/...`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Then run `make manifests generate helm-crds`.
- [ ] **Step 4: Run `go test ./test/admission/ ./internal/cli/ ./api/... && make helm-lint && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(api): suggestion admission, immutability and rule authors"`

---

### Task 4: Gateway: LLM purpose and authorizedBy in QueryAudit

**Files:**
- Modify:
  - `proto/last/gateway/v1/gateway.proto`:
    - `CallLLMRequest.purpose` (string, field 5);
    - `message Authorization { string kind = 1; string name = 2; }`;
    - `AuditEntry.authorized_by` (field 7).
    - Then run `make proto`.
  - `internal/gateway/llmrpc.go`:
    - refuse a purpose other than `""`, `turn` or `suggest` with InvalidArgument;
    - record `purpose` in both payloads;
    - the scripted provider gets the purpose.
  - `internal/gateway/llm/scripted.go`: `Request` carries `Purpose`; `suggest` reads the key `"_suggest." + ScriptKey(intent)`.
  - `internal/gateway/index.go`: `observe` skips `llm.request` and `llm.response` entries whose payload purpose is `suggest`.
  - `internal/gateway/queryaudit.go`: fill `authorized_by` from `Entry.AuthorizedBy`.
  - `internal/controller/fakegateway_test.go`: serve `authorized_by`, and answer `purpose: suggest` from a per-Intent suggest script.
  - Main spec §12 and §14.

**Interfaces:**
- Produces:
  - `gatewayv1.CallLLMRequest.Purpose`;
  - `gatewayv1.AuditEntry.AuthorizedBy *gatewayv1.Authorization`;
  - `const PurposeSuggest = "suggest"` in `internal/gatewayapi/gatewayv1` (a hand-written file beside `refusals.go`);
  - the fake gateway's `suggestScript map[string][]string` (Intent key to answers) and its `calls` log of purposes.

- [ ] **Step 1: Write the failing tests.**
  - `TestSuggestCallsAreAuditedButNotIndexed`: a suggest call writes `llm.request` and `llm.response` carrying `"purpose":"suggest"`, and QueryAudit for `llm.request` doesn't return them.
  - `TestScriptedSuggestReadsItsOwnKey`: with `_suggest.ns.a` present, that answer is returned; without it, `ErrNoScript`.
  - `TestUnknownPurposeIsRefused`.
  - `TestQueryAuditCarriesAuthorizedBy`: an approval-authorized `command.start` returns `{approval, <IntentApproval name>}`.
  - `internal/agent`: `TestReloadIgnoresSuggestCalls`. A suggest request between two turns doesn't change the rebuilt conversation; the index skip makes this hold.
- [ ] **Step 2: Run `go test ./internal/gateway/... ./internal/agent/`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test -race ./internal/gateway/... ./internal/agent/ && make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(gateway): tag suggest LLM calls and expose authorizedBy in QueryAudit"`

---

### Task 5: Gateway: `suggestion.decided`

**Files:**
- Create: `internal/gateway/suggestions.go`, `internal/gateway/suggestions_test.go`.
- Modify: `internal/gateway/server.go`: a 30s ticker in `lead` calls `g.recordDecisions(ctx, w)`.

**Interfaces:**
- Produces: `func (g *Gateway) recordDecisions(ctx context.Context, w *audit.Writer) error`.
  - It lists suggestions, and for each one decided whose UID is not in the ConfigMap `last-suggestion-decisions` (key: UID; value: the entry's seq), it appends `suggestion.decided`.
  - Payload: `{targetScope, namespace, name, uid, resourceVersion, trigger, decision, decidedBy, decisionReason, proposed (canonical JSON text), proposedHash "sha256:<hex>"}`. There is no Intent, and the actor is `gateway`.
  - After appending, it saves the ConfigMap.

- [ ] **Step 1: Write the failing tests.**
  - `TestDecidedSuggestionIsAuditedOnce`: two runs write one entry, and its payload fields equal the object's.
  - `TestPendingSuggestionIsNotAudited`.
  - `TestRecordedDecisionsSurviveARestart`: a new Gateway with the same ConfigMap writes nothing.
  - `TestAWriteFailureLeavesTheUIDUnrecorded`: the next run writes it.
- [ ] **Step 2: Run `go test ./internal/gateway/ -run Suggestion`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test -race ./internal/gateway/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(gateway): audit suggestion decisions"`

---

### Task 6: Operator: PlanTime suggestions and the shared creator

**Files:**
- Create:
  - `internal/controller/suggestions.go`: the shared creator.
  - `internal/controller/intent_suggest.go`: the PlanTime step.
  - `internal/controller/intent_suggest_test.go`.
- Modify:
  - `internal/controller/intent_planning.go`: `awaitApproval` calls `r.planTime(ctx, in)` before reading the decision, when `!in.Status.Plan.Suggested`.
  - `internal/controller/intent_controller.go`: `IntentReconciler` gains `Suggestions *SuggestionCreator`.
  - `cmd/operator/main.go`: wire it up, and add the `--system-namespace` flag.

**Interfaces:**
- Consumes:
  - `suggest.Vet`, `Name`, `Request`, `Parse`, `Schema` and `SystemPrompt` (Task 2);
  - `rules.PlanCoverage` and `FromObjects`;
  - `gatewayv1.PurposeSuggest` (Task 4);
  - `Plan.Suggested` (Task 3).
- Produces:
  - `type SuggestionCreator struct{ Client client.Client; Reader client.Reader; Recorder events.EventRecorder; SystemNamespace string; MaxPending int }`.
  - `func (c *SuggestionCreator) Create(ctx context.Context, p suggest.Proposal, intent *lastv1alpha1.Intent) (name string, created bool, err error)`:
    - reads rules and Denied suggestions uncached;
    - runs Vet;
    - counts Pending in the namespace against `MaxPending` (100);
    - creates with the label;
    - treats AlreadyExists as found;
    - emits the §8 events.
  - `func (c *SuggestionCreator) Ask(ctx context.Context, gw gatewayv1.GatewayServiceClient, ref *gatewayv1.IntentRef, entries []suggest.Entry) ([]suggest.Answer, error)`: one `CallLLM` with `purpose: suggest`.
  - `func (r *IntentReconciler) planTime(ctx context.Context, in *lastv1alpha1.Intent) error`:
    - asks the LLM for the plan's uncovered entries;
    - builds proposals with the §4.1 stages and M6 R13 providers, egress and install (reuse `internal/cli`'s logic by moving `providersFor` and the R13 spec builder into `internal/suggest` as `func RuleSpecFor(body plan.Body, entryID string, cmd lastv1alpha1.Command, stages []lastv1alpha1.Stage, extraProviders []string) (lastv1alpha1.CommandRuleSpec, bool)`, and update `internal/cli` to call it);
    - creates them;
    - patches `status.plan.suggested=true` and `status.suggestedRules`.
  - The plan step is skipped for plans that never wait.

- [ ] **Step 1: Write the failing envtest specs.**
  - "creates a PlanTime suggestion per uncovered entry with Remediate added for a Create plan":
    - stages `[Create, Remediate]` for a step;
    - `[Delete]` for a teardown entry;
    - the names are in `suggestedRules`;
    - `suggested` is true.
  - "asks the LLM once per plan": a second reconcile makes no second suggest call.
  - "drops a code-varying answer with SuggestionDropped".
  - "suppresses a proposal broader than a denied one with SuggestionSuppressed naming it".
  - "suppresses a proposal identical to a denied one from another trigger" (Review Focus 5).
  - "creates a narrower proposal than a denied one".
  - "stops at 100 pending suggestions with SuggestionLimit".
  - "an LLM failure raises SuggestionFailed, marks the plan suggested, and the Intent still waits for approval".
  - "the Intent deleted during the suggest call ends the step without error" (Review Focus 4).
  - "makes no suggest call for a plan that runs without approval".
- [ ] **Step 2: Run `make test` focused with `-ginkgo.focus suggest`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Update main spec §9.5 with the PlanTime details, S6 and S8.
- [ ] **Step 4: Run `make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(operator): PlanTime rule suggestions"`

---

### Task 7: Operator: RepeatedApproval and PeriodicReview

**Files:**
- Create: `internal/controller/suggest_runner.go`, `internal/controller/suggest_runner_test.go`.
- Modify:
  - `internal/controller/intent_execution.go`: after a successful `EndPhase`, call `r.Runner.Queue(in.Namespace)`.
  - `cmd/operator/main.go`: `mgr.Add(runner)`, which runs on the leader only.

**Interfaces:**
- Consumes: `SuggestionCreator` (Task 6), `rules.GroupKey` and `Merge` (Task 1), `AuditEntry.AuthorizedBy` (Task 4), `OperatorConfig.spec.suggestions`.
- Produces:
  - `type SuggestRunner struct{ Creator *SuggestionCreator; Gateway gatewayv1.GatewayServiceClient; Client client.Client; Now func() time.Time; Debounce time.Duration }`, with `Debounce` 1 minute.
  - `func (s *SuggestRunner) Queue(namespace string)`.
  - `func (s *SuggestRunner) Start(ctx context.Context) error`.
  - `func (s *SuggestRunner) ScanRepeated(ctx context.Context, namespace string) error`.
  - `func (s *SuggestRunner) Review(ctx context.Context) error`.

- [ ] **Step 1: Write the failing envtest specs (fake gateway audit entries).**
  - "suggests after 3 distinct approvals of a group and not after 2". The group's commands are spread over 2 and then 3 Intents.
  - "counts the same IntentApproval once, however many commands it ran".
  - "ignores rule- and Auto-authorized commands".
  - "honours repeatThreshold from OperatorConfig".
  - "falls back to Merge when the LLM gives no answer, and makes no suggestion when neither works, with SuggestionFailed".
  - "the LLM call uses the newest example Intent".
  - "Review merges two CommandRules that differ in one position into a suggestion that contains both". The rationale names both rules.
  - "Review does not merge rules that differ at a StartsCode position".
  - "Review proposes a Cluster suggestion in last-system for identical rules in two namespaces".
  - "Queue debounces scans to one per minute per namespace" (with an injected clock).
- [ ] **Step 2: Run `make test` focused with `-ginkgo.focus Repeated|Review|Queue`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
  - QueryAudit pages with `after_seq` over each Intent in the namespace, with types `command.start`.
  - Examples are the group's 20 newest commands.
  - The review interval comes from OperatorConfig (default 24h), with ±10% jitter.
  - Update main spec §9.5 with the trigger details from design §4.2 and §4.3.
- [ ] **Step 4: Run `make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(operator): RepeatedApproval and PeriodicReview suggestions"`

---

### Task 8: CLI: deciding suggestions

**Files:**
- Create: `internal/cli/suggestions.go`, `internal/cli/suggestions_test.go`.
- Modify:
  - `internal/cli/rules.go`: `newRulesCommand` adds `suggestions`; `chooseRule` offers `s`; `printCoverage` lists covering suggestions.
  - `internal/cli/plan.go`, if `printCoverage` lives there.
  - `README.md`: the suggestions section, and "Upgrading to M7" (design §12).

**Interfaces:**
- Consumes: `suggest.Vet`'s lint parts, `Broader`, `Sanitize` and `RuleSpecFor`; `rules.Covers`; `RuleSource.CreatedBy`.
- Produces:
  - `kubectl last rules suggestions [-n ns | -A] [--all]`;
  - `show <name>`;
  - `accept <name> [--edit] [--yes]`;
  - `deny <name> [--reason text]`;
  - `func coveringSuggestions(ctx context.Context, k *Kube, ns string, stage lastv1alpha1.Stage, cmd lastv1alpha1.Command) ([]lastv1alpha1.CommandRuleSuggestion, error)` (S7).

- [ ] **Step 1: Write the failing tests (fake client, as in `rules_test.go`).**
  - `TestListShowsPendingWithOwnLint`: the CLI's lint count is shown, not `breadthWarnings`.
  - `TestShowLabelsOperatorTextAndBreadth`:
    - "operator-proposed" is shown;
    - each `Broader` line is shown;
    - examples are marked as matching or not matching;
    - sanitized text appears.
  - `TestAcceptCreatesTheRuleThenDecides`:
    - the rule is named after the suggestion, in the suggestion's namespace (a ClusterCommandRule for Cluster);
    - its source is `{suggestion, ref, createdBy}`;
    - then `Accepted` with `decidedBy` set.
  - `TestAcceptEditRelintsAndRecordsTheEdit`.
  - `TestAcceptRerunAfterAFailedDecisionFinishes`.
  - `TestConcurrentAcceptReportsTheOtherDecider` (Review Focus 1).
  - `TestAcceptWithoutRuleRBACLeavesItPending` (Review Focus 2).
  - `TestAcceptRefusesCodeVaryingOrMatchNothing`.
  - `TestDenySetsReasonAndDecider`.
  - `TestApproveOffersSuggestedChoice`: the interactive `s` accepts the suggestion and then creates the IntentApproval.
  - `TestBatchAlwaysAllowNeverUsesSuggestions`.
  - `TestTerminalEscapesAreStripped` (Review Focus 3).
- [ ] **Step 2: Run `go test ./internal/cli/`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Update main spec §9.5 "Deciding".
- [ ] **Step 4: Run `go test ./internal/cli/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(cli): list, show, accept and deny rule suggestions"`

---

### Task 9: e2e and close-out

**Files:**
- Create: `test/e2e/suggestions_e2e_test.go`.
- Modify:
  - the e2e script helper (`day2.scriptFor` in `test/e2e`): it can also write a `_suggest.<ns>.<name>` key;
  - main spec §20 (M7 done) and any §11 amendment still missing.

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Write `TestDeniedSuggestionIsNotRepeated`** as in design §10.
  - Intents A, B and C share one plan shape.
  - P is broad, P⁺ is broader than P, and N is narrower: N has a literal where P has `[!-]*`.
  - The test asserts:
    - A's suggestion exists, and `rules suggestions deny --reason` makes it Denied with `decidedBy` set to the kind user;
    - B gets two `SuggestionSuppressed` events naming A's suggestion, and no new suggestion;
    - C's N exists, and `accept --yes` creates the rule with `source.createdBy` set to the kind user;
    - QueryAudit cannot see `suggestion.decided` (it has no Intent), so the test reads the chain with `kubectl last audit log` and finds two `suggestion.decided` entries;
    - `kubectl last audit verify` passes.
- [ ] **Step 2: Run it on kind `last-m4a` only.** Run `go test ./test/e2e/ -run TestDeniedSuggestionIsNotRepeated -timeout 40m`. Expected: PASS.
- [ ] **Step 3: Run the full e2e suite on `last-m4a`.** Expected: every test passes.
- [ ] **Step 4: Commit.** `git commit -m "test(e2e): a denied suggestion is never re-suggested; a narrower one is"`
