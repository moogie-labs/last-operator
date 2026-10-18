package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/probe"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// ProbeRunRequest is a set of probes for one Intent.
type ProbeRunRequest struct {
	// Actor names the caller the probes run for. It is required and is recorded on
	// probe.start and probe.result. M2b fills it from gateway.CallerFrom(ctx).
	Actor  string
	Intent string
	Probes []lastv1alpha1.Probe
	// Images maps ImageKey to the image reference its group runs on; "" or a missing
	// key means the executor's configured image.
	Images map[string]string
}

// ImageKey identifies a command's image spec for grouping: "" without install, else
// the canonical JSON of its install list.
func ImageKey(c lastv1alpha1.Command) string {
	if c.Image == nil || len(c.Image.Install) == 0 {
		return ""
	}
	b, err := audit.Canonical(c.Image.Install)
	if err != nil { // an argv list always marshals; keep distinct specs distinct anyway
		return fmt.Sprintf("%#v", c.Image.Install)
	}
	return string(b)
}

// ProbeOutcome is one probe's result. Passed is false whenever Error is set.
type ProbeOutcome struct {
	ID     string
	Passed bool
	Result runnerapi.Result
	Error  string
}

type probeRef struct {
	ID      string               `json:"id"`
	Command lastv1alpha1.Command `json:"command"`
	Assert  string               `json:"assert"`
}

type probeStart struct {
	RunID       string             `json:"runId"`
	Image       string             `json:"image"`
	Probes      []probeRef         `json:"probes"`
	Providers   []string           `json:"providers,omitempty"`
	Egress      []string           `json:"egress,omitempty"`
	Credentials []IssuedCredential `json:"credentials,omitempty"`
}

type probeResultEntry struct {
	ID              string `json:"id"`
	ExitCode        int    `json:"exitCode"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated"`
	StderrTruncated bool   `json:"stderrTruncated"`
	Passed          bool   `json:"passed"`
	Error           string `json:"error,omitempty"`
}

type probeResultPayload struct {
	RunID         string             `json:"runId"`
	StartSeq      uint64             `json:"startSeq"`
	Results       []probeResultEntry `json:"results"`
	Error         string             `json:"error,omitempty"`
	EgressRevoked bool               `json:"egressRevoked"`
	CleanupError  string             `json:"cleanupError,omitempty"`
}

// RunProbes runs probes grouped by identical providers, egress and image, one pod per group,
// each recorded as a single probe.start/probe.result pair (spec §10.2).
func (e *Executor) RunProbes(ctx context.Context, req ProbeRunRequest) ([]ProbeOutcome, error) {
	if req.Actor == "" {
		return nil, errors.New("sandbox: probe request has no actor")
	}
	seen := map[string]bool{}
	for _, p := range req.Probes {
		if seen[p.ID] {
			return nil, fmt.Errorf("sandbox: duplicate probe id %q", p.ID)
		}
		seen[p.ID] = true
	}
	byID := map[string]ProbeOutcome{}
	var errs []error
	for _, group := range groupProbes(req.Probes) {
		ge := e
		if img := req.Images[ImageKey(group[0].Command)]; img != "" {
			c := *e
			c.cfg.Pod.Image = img
			ge = &c
		}
		outs, err := ge.runProbeGroup(ctx, req.Actor, req.Intent, group)
		for _, o := range outs {
			byID[o.ID] = o
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	outcomes := make([]ProbeOutcome, 0, len(req.Probes))
	for _, p := range req.Probes {
		outcomes = append(outcomes, byID[p.ID])
	}
	return outcomes, errors.Join(errs...)
}

// groupProbes groups probes by the set of their providers, the set of their egress
// (order and duplicates ignored) and their image spec, keeping first-seen order.
func groupProbes(probes []lastv1alpha1.Probe) [][]lastv1alpha1.Probe {
	var keys []string
	groups := map[string][]lastv1alpha1.Probe{}
	for _, p := range probes {
		key, _ := json.Marshal(struct {
			P, E []string
			I    string
		}{uniqueSorted(p.Command.Providers), uniqueSorted(p.Command.Egress), ImageKey(p.Command)})
		if _, ok := groups[string(key)]; !ok {
			keys = append(keys, string(key))
		}
		groups[string(key)] = append(groups[string(key)], p)
	}
	out := make([][]lastv1alpha1.Probe, 0, len(keys))
	for _, k := range keys {
		out = append(out, groups[k])
	}
	return out
}

func uniqueSorted(xs []string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return slices.Compact(out)
}

const maxAssertErrorLen = 1024

// truncateError cuts s to maxAssertErrorLen bytes on a UTF-8 boundary.
func truncateError(s string) string {
	if len(s) <= maxAssertErrorLen {
		return s
	}
	cut := maxAssertErrorLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}

func (e *Executor) runProbeGroup(ctx context.Context, actor, intent string, probes []lastv1alpha1.Probe) ([]ProbeOutcome, error) {
	outcomes := make([]ProbeOutcome, len(probes))
	failAll := func(err error) ([]ProbeOutcome, error) {
		for i, p := range probes {
			if outcomes[i].ID == "" {
				outcomes[i] = ProbeOutcome{ID: p.ID, Result: runnerapi.Result{ID: p.ID, ExitCode: -1}, Error: err.Error()}
			}
		}
		return outcomes, err
	}
	cmds := make([]lastv1alpha1.Command, len(probes))
	refs := make([]probeRef, len(probes))
	for i, p := range probes {
		c := p.Command
		c.ID = p.ID
		cmds[i] = c
		refs[i] = probeRef{ID: p.ID, Command: p.Command, Assert: p.Assert}
	}
	providerNames := uniqueSorted(probes[0].Command.Providers)
	// Take the slot first so a queued group reads fresh Provider and API server data.
	release, err := e.sched.Acquire(ctx, PriorityProbe)
	if err != nil {
		return failAll(err)
	}
	defer release()
	prep, err := e.prepare(ctx, cmds, providerNames, uniqueSorted(probes[0].Command.Egress))
	if err != nil {
		return failAll(err)
	}

	runID := e.cfg.NewRunID()
	issued := make([]IssuedCredential, 0, len(prep.plans))
	for _, cp := range prep.plans {
		issued = append(issued, cp.Issued)
	}
	draft := func(typ string, payload any) audit.Draft {
		return audit.Draft{Type: typ, Actor: actor, Intent: intent, Payload: payload}
	}
	var cleanupErr string
	var revoked bool
	runCtx := ctx
	// The result entry must be written even when the request was cancelled, so the
	// audit calls ignore cancellation while the run itself still honors it.
	err = e.auditor.Do(context.WithoutCancel(ctx),
		draft(audit.TypeProbeStart, probeStart{RunID: runID, Image: e.cfg.Pod.Image, Probes: refs,
			Providers: providerNames, Egress: ruleStrings(prep.rules), Credentials: issued}),
		func(context.Context) (any, error) {
			ctx := runCtx
			results, runErr := e.launch(ctx, runID, prep, launchHooks{})
			if cerr := e.cleanup(ctx, runID, func(context.Context) error { revoked = true; return nil }); cerr != nil {
				cleanupErr = cerr.Error()
			}
			if runErr != nil {
				return nil, runErr
			}
			byID := map[string]runnerapi.Result{}
			for _, r := range results {
				byID[r.ID] = r
			}
			for i, p := range probes {
				r, ok := byID[p.ID]
				o := ProbeOutcome{ID: p.ID, Result: r}
				switch {
				case !ok:
					o.Result = runnerapi.Result{ID: p.ID, ExitCode: -1}
					o.Error = "sandbox: no result for probe"
				case r.Error != "":
					o.Error = r.Error
				default:
					passed, aerr := probe.Evaluate(ctx, p.Assert, probe.Result{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr})
					o.Passed = passed && aerr == nil
					if aerr != nil {
						o.Error = truncateError(aerr.Error())
					}
				}
				outcomes[i] = o
			}
			return nil, nil
		},
		func(start audit.Entry, _ any, actionErr error) audit.Draft {
			p := probeResultPayload{RunID: runID, StartSeq: start.Seq, EgressRevoked: revoked, CleanupError: cleanupErr}
			if actionErr != nil {
				p.Error = actionErr.Error()
			}
			for i, o := range outcomes {
				if o.ID == "" { // the action failed before this probe was judged
					msg := "sandbox: probe did not run"
					if actionErr != nil {
						msg = actionErr.Error()
					}
					o = ProbeOutcome{ID: probes[i].ID, Result: runnerapi.Result{ID: probes[i].ID, ExitCode: -1}, Error: msg}
				}
				p.Results = append(p.Results, probeResultEntry{ID: o.ID, ExitCode: o.Result.ExitCode,
					Stdout: o.Result.Stdout, Stderr: o.Result.Stderr, StdoutTruncated: o.Result.StdoutTruncated,
					StderrTruncated: o.Result.StderrTruncated, Passed: o.Passed, Error: o.Error})
			}
			return draft(audit.TypeProbeResult, p)
		})
	if err != nil {
		return failAll(err)
	}
	return outcomes, nil
}
