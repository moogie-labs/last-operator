# M3c (`kubectl last` and the demo) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-15-m3c-cli-and-demo.md`.

## Before M3 is done

- **Run the kind e2e.** Done on 2026-10-16 at 48cd4cf: `KIND_CLUSTER=last-e2e-m2a hack/e2e.sh` passed every test, with `ok test/e2e 376s`. `TestOperatorWebServer` passed in 124s. It drove `kubectl last` through a question, a discovery command and the plan to Ready, and verified the audit chain.
- **Run the demo.** `ANTHROPIC_API_KEY=… make demo 2>&1 | tee demo.log` runs the M3 done-criterion against Claude. It also confirms three things nothing else has checked:
  - that Anthropic structured outputs accept the turn schema (M3b follow-ups);
  - that fallback text is joined correctly (M3a follow-ups);
  - that 64000 `max_tokens` and the 30-minute per-call bound hold up.

  Record the transcript here.

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
