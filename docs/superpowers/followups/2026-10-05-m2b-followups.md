# M2b (gateway lifecycle API) follow-ups

Deferred findings from the per-task and final whole-branch reviews of
`docs/superpowers/plans/2026-10-04-m2b-gateway-lifecycle-api.md`. Items fixed
by the final fix wave (W1–W6) are not listed.

## M2c (operator side) must respect or build

- **Operator RBAC excludes:**
  - Secrets anywhere (the output-Secret ownership check relies on it);
  - ConfigMaps or Secrets in `last-gateway`;
  - pods or `pods/log` in the sandbox namespace;
  - `impersonate`;
  - `serviceaccounts/token`;
  - create, update or delete on `intentapprovals`;
  - writes to IntentPolicy, OperatorConfig, Provider and LLMProvider;
  - any RBAC or admission-policy writes.

  Grant `delete` on ManagedResources only deliberately: deleting an MR
  garbage-collects its output Secret with no audit entry.
- **gRPC error handling:**
  - `Aborted`: retry (for probes, skip and count the skip).
  - `AlreadyExists`: end the existing phase first.
  - `Unavailable`: back off; retry EndPhase with the same outcome.
  - `FailedPrecondition` "no open phase" after a gateway restart: the phase was interrupted. Replan; the same subject never reopens.
  - `PermissionDenied` mid-phase: an approval was withdrawn or policy narrowed (W1). End the phase as failed.
- **Plans:**
  - Compute `status.plan.hash` with `internal/plan.Hash`.
  - At least one step; unique step IDs; unique probe IDs.
  - Stay well under the size bound (`MaxRecordBytes`).
- **ManagedResources:** each needs a controller ownerRef to the Intent UID plus `spec.intentRef`. `outputsSecretRef` must be a name the plan declares. WriteOutputSecret values are only `${steps…}` expressions.
- **Also to build:**
  - at most one RunCommand per Intent at a time;
  - the 9090 ingress `podSelector` for the operator pod;
  - fixed-interval probe scheduling with the skip metric;
  - the operator Deployment and its projected token (audience `last-gateway`);
  - the scripted-LLM web-server e2e.
- **Intent identity:** decide on Intent deletion and recreation. Approvals and gateway records are keyed by namespace and name, not UID, so a recreated Intent with the same name inherits `Used` and `Executed`.

## Before M5 (Update / Remediate / Delete stages)

- **Approval replay.** Once a subject drops out of the record's last-50 `Used` list, an old immutable approval could authorize the same plan again. Tie approvals to the Intent UID and require approval freshness (e.g. `creationTimestamp` after the record's last begin).
- **Close interrupted phases eagerly at lead.** List `component=intent-state` ConfigMaps and close every `Open` from another epoch, instead of waiting for that Intent's next BeginPhase.
- **EndPhase Save failure plus restart** writes an `interrupted` `phase.end` after a `succeeded` one, and `Executed` is never recorded. A leader-start reconciliation from the chain would close this.

## Security hardening

- **Dedicated namespace.** Move per-run anchors and Provider ServiceAccounts and Secrets to their own namespace (e.g. `last-gateway-runs`). Then the gateway Role in `last-gateway` can return to `resourceNames` rules. This is the structural form of the audit-object guard.
- **Narrow output-Secret writes.** The cluster-wide Secret get/create/update for output Secrets is broad. Consider an admission policy that limits gateway writes outside `last-gateway` to Secrets carrying the gateway's labels and ownerRefs.
- **Public key ConfigMap.** `last-audit-pubkey` stays gateway-writable, and it is the verifier trust anchor.
- **Failed output-Secret write.** It records `output.written` but no outcome entry; consider `Do` with a result entry.
- **Probe entries** carry no `SubjectHash` or `AuthorizedBy` for the executed plan.
- **Sensitive outputs** reach the operator through RunCommand stdout and are logged in full in `command.result` (spec §14.2/§15 design). Revisit before real secrets flow through outputs.

## Tests and polish

- Plan: float-equivalence hash test; empty probe ID allowed by `Validate`.
- Policy: untested IntentPolicy-layer cases; `CheckCommand` wraps a parse error with `%v`.
- Anchors: executor-level tests for anchor cleanup on failure paths; reactor UID realism.
- Runtime:
  - scheduler sizing from `maxConcurrent` untested;
  - IPv6 canary config cases missing;
  - a tiny window where `Run` skips the drain wait;
  - a leaked `Wait` goroutine on drain timeout.
- State store: Conflict and AlreadyExists tests; guard against an empty Intent.
- Phases:
  - duplicate `phase.end` after an ambiguous Save in `closeInterrupted`;
  - no test for a `policy.denied` append failure.
- Output Secrets:
  - a non-NotFound Get maps to Unavailable;
  - within one Intent, one MR can claim another MR's planned Secret name first;
  - an `immutable: true` existing Secret fails after the audit entry.
- CallLLM:
  - provider error plus failed outcome append maps to FailedPrecondition;
  - negative `MaxTokens` unvalidated (fix with the M3 adapters);
  - document that scripted mode is test-only.
- Admission:
  - DELETE on IntentApprovals isn't covered (keep approver RBAC without delete);
  - the immutable assertion also matches the CRD message;
  - no envtest for the external-mode audit guard;
  - guarded names apply in every namespace.
- Chart: render checks are substring greps (ClusterRole and namespaces unchecked).
- e2e: a non-NotFound OperatorConfig Get is swallowed in cleanup.
