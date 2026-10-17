# M3c (`kubectl last` and the demo) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-15-m3c-cli-and-demo.md`.

## Before M3 is done

- **Run the kind e2e.** Done on 2026-10-16 at 48cd4cf: `KIND_CLUSTER=last-e2e-m2a hack/e2e.sh` passed every test, with `ok test/e2e 376s`. `TestOperatorWebServer` passed in 124s. It drove `kubectl last` through a question, a discovery command and the plan to Ready, and verified the audit chain.
- **Run the demo.** Done on 2026-10-17 at 72ad33b: M3's done-criterion passed against `claude-opus-5-5`. See "Demo result (2026-10-17)" below.

## Demo result (2026-10-16): blocked by the turn schema, then fixed

The first `make demo` failed on its first turn. Anthropic answered HTTP 400: "The compiled grammar is too large, which would cause performance issues." The cluster, chart, key Secret and LLMProvider all worked.

On 2026-10-17, `hack/schemaprobe` tested five variants against the real API:

| Variant | Size | Result |
|---|---|---|
| Today's schema | 3621 bytes | rejected |
| `command` as a plain object, not "object or null" | 3593 bytes | rejected |
| Shared objects in `$defs` | 2069 bytes | accepted |
| `$defs`, without the union | 2041 bytes | accepted |
| Without resources | 1722 bytes | accepted |

The cause was writing the command object out in full four times, not the union. `agent.Schema()` now defines the command, probe and resource objects once in `$defs` and references them. It is byte-for-byte the accepted variant. `TestSchemaDefinesEachSharedObjectOnce` pins this, and `go run ./hack/schemaprobe` checks the schema against the real API. The rerun passed (below).

## Demo result (2026-10-17): passed

`ANTHROPIC_API_KEY=… make demo` on the reused `last-demo` cluster, at 72ad33b, with `claude-opus-5-5` at effort high. The CLI part of the transcript, with the plan's resources shortened:

```
Creating an Intent in namespace demo from: run an nginx web server serving a hello page
Created Intent demo/run-an-nginx-web-server-wrqq
Intent demo/run-an-nginx-web-server-wrqq: Pending
Intent demo/run-an-nginx-web-server-wrqq: Planning (Asking the LLM for a plan)
Intent demo/run-an-nginx-web-server-wrqq: AwaitingApproval (Waiting for an IntentApproval of plan sha256:4f0ed9ba… for stage Create)

Plan sha256:4f0ed9ba… (stage Create)
Summary: Deploy nginx (nginx:1.27-alpine) in namespace demo serving a hello page from a ConfigMap, exposed by ClusterIP Service hello-nginx on port 80
Steps:
  1. s1: Create the hello page ConfigMap, the nginx Deployment serving it, and a ClusterIP Service
     run:       kubectl apply -n demo -f /work/hello-nginx.yaml
     file:      /work/hello-nginx.yaml (1277 bytes)
     providers: demo-kubernetes
     timeout:   2m0s
  2. s2: Wait until the nginx Deployment is rolled out and ready
     run:       kubectl rollout status deployment/hello-nginx -n demo --timeout=180s
     providers: demo-kubernetes
     timeout:   4m0s
Resources:
  - ConfigMap demo/hello-nginx-html holding the hello page            (probe: kubectl get configmap …)
  - nginx Deployment demo/hello-nginx                                  (probe: readyReplicas >= 1)
  - ClusterIP Service demo/hello-nginx on port 80                      (probe: endpoints have addresses)
Approve? [y/N/d/f]: Approved plan sha256:4f0ed9ba… as kubernetes-admin (IntentApproval run-an-nginx-web-server-wrqq-create-65ckd)
Intent demo/run-an-nginx-web-server-wrqq: Executing (Running plan sha256:4f0ed9ba…)
Intent demo/run-an-nginx-web-server-wrqq: Verifying (Running every probe once)
Intent demo/run-an-nginx-web-server-wrqq: Ready (Every probe passed)
Intent demo/run-an-nginx-web-server-wrqq is Ready.
  ManagedResource run-an-nginx-web-server-wrqq-0: ConfigMap demo/hello-nginx-html holding the hello page
  ManagedResource run-an-nginx-web-server-wrqq-1: nginx Deployment demo/hello-nginx
  ManagedResource run-an-nginx-web-server-wrqq-2: ClusterIP Service demo/hello-nginx on port 80

Verifying the audit chain
OK: verified 24 entries (seq 0..23), head sha256:884e8be0…
```

What the audit chain shows, and the three open checks:

- **The model planned in one turn.** It asked no questions and ran no discovery command, so the real model did not exercise Clarifying or Discovery; the kind e2e covers both with the scripted model. Entries 6 and 7 are the only `llm.request` and `llm.response`, with no correction turn: the first answer parsed and passed the plan checks.
- **The turn schema.** Anthropic accepted the `$defs` schema.
- **`max_tokens` 64000.** Accepted; the request records `maxTokens: 64000`.
- **The 30-minute call bound.** Not approached: the call took 19.8 seconds. It is still untested against a long turn.
- **Fallback text joining.** Not exercised: no fallback happens unless the requested model declines, and a benign request won't make it. `TestAnthropicJoinsTheTextOnEitherSideOfAFallback` now pins the join instead.
- **Execution and probes.** Two steps ran with one short-lived credential and egress grant each, both revoked on completion; all three probes passed.

`planningSessionSeq` reads 6 because it is the audit sequence number of the session's first `llm.request`, not a count of sessions. The approval came about six hours after the plan; the operator waited in AwaitingApproval without re-planning.

The script printed `Terminated: 15` when its EXIT trap killed the port-forward. The trap now reaps it with `wait`.

## Approver display

- **Unhashed prose.** `Plan.Summary` and `PendingCommand.Reason` are shown, but they sit outside the hashed body (main spec §8.2). The operator can therefore change them; the commands and resources an approval covers it cannot. Label that prose as not covered by the hash. Also tighten the wording in M3 spec §5 and the README: the CLI recomputes the hash "from the commands and resources it shows", not from everything it shows.
- **Status line.** `printStatus` shows the operator-written Ready message, which can name any hash. The text is escaped and never offered as a command to run, but it can mislead.
- **Checks the gateway makes that the CLI does not.** The CLI does not repeat the gateway's discovery checks (step id, nonce, size) or `plan.Validate`. It can therefore show a subject as decidable that the gateway will refuse. That is harmless: an approval of it runs nothing.
- **M4 image builds.** `renderCommand` shows only how many install commands an `image.install` holds. M4 must render the install commands themselves.
- **Text that can mislead.** Runs of whitespace in prose fields can make text look like a separate `run:` line. Collapse them.
- **Ambiguous escapes:**
  - `clean` does not double backslashes, so a literal `\u001b` looks like an escaped character;
  - runes above U+FFFF are written with more than 4 hex digits.
- **Trailing `|` line.** File contents that end in a newline print an extra empty `  | ` line.

## CLI behaviour

- **Following.** Any `Get` error ends the follow, and a deleted Intent shows a raw NotFound. There is no command to resume following (`kubectl last follow NAME`).
- **Ready output.** A Ready Intent prints two status lines. A Failed Intent with no condition prints `failed: : `.
- **Listing ManagedResources.** They are matched by `intentRef.name` only, so a recreated Intent could list old ones.
- **`approve`/`deny`:**
  - A failed re-read after `Create` returns an error although the decision was recorded, so a script that retries records a duplicate.
  - The deny reason is stored untrimmed.
  - The "moved on" note doesn't say what kind of subject the Intent now asks about.
- **Input.**
  - The prompter's scanner has a 64 KiB line limit and ignores `s.Err()`.
  - The prompter reads ahead on stdin, so only one may exist per process.
  - The output says "Recorded 1 answers.".
  - A concurrent answer for the same id is overwritten silently.
- **Flags.** The kube flags are persistent on the root command, so `audit` accepts `-n`, `--context` and `--kubeconfig` and ignores them.
- **Gateway wording.** `RunCommand`'s "no open phase for this plan" can reach the model for a discovery command. The replay message says "again" twice.

## Demo

- **Key in an old annotation.** On a reused cluster where an earlier `hack/demo.sh` created the key Secret with client-side apply, the last-applied annotation may still hold the key. Delete the Secret before applying.
- **Script robustness:**
  - A port-forward failure is silent.
  - A failed or declined run skips `audit verify`.
  - `$NS` is interpolated into YAML unquoted.

## Tests

- **Missing CLI tests:**
  - denying a plan;
  - Ctrl-C while a prompt blocks;
  - `NewKube` with a temporary kubeconfig;
  - conflict retries in `answerQuestions`.
- **e2e assertions.** The discovery-hash checks in the e2e are substring-only.
