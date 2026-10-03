package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// Auditor is the part of *audit.Writer the executor needs.
type Auditor interface {
	Append(ctx context.Context, d audit.Draft) (audit.Entry, error)
	Do(ctx context.Context, before audit.Draft, action func(context.Context) (any, error),
		after func(before audit.Entry, result any, actionErr error) audit.Draft) error
}

// LogReader returns the runner container's log.
type LogReader func(ctx context.Context, namespace, pod string) ([]byte, error)

// KubeLogReader reads pod logs through the API server.
func KubeLogReader(kube kubernetes.Interface) LogReader {
	return func(ctx context.Context, namespace, pod string) ([]byte, error) {
		return kube.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: "runner"}).DoRaw(ctx)
	}
}

// ExecutorConfig configures sandbox runs.
type ExecutorConfig struct {
	Pod               PodConfig
	ProviderNamespace string
	PollInterval      time.Duration // default 1s
	DeadlineOverhead  time.Duration // added to the summed command timeouts for activeDeadlineSeconds; default 60s
	StartupGrace      time.Duration // extra wait for scheduling and image pull; default 2m
	CleanupTimeout    time.Duration // default 30s
	NewRunID          func() string // default: 32 random hex characters
}

func (c ExecutorConfig) withDefaults() ExecutorConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.DeadlineOverhead <= 0 {
		c.DeadlineOverhead = 60 * time.Second
	}
	if c.StartupGrace <= 0 {
		c.StartupGrace = 2 * time.Minute
	}
	if c.CleanupTimeout <= 0 {
		c.CleanupTimeout = 30 * time.Second
	}
	if c.NewRunID == nil {
		c.NewRunID = randomRunID
	}
	return c
}

// Executor runs commands in sandbox pods with fail-closed audit (spec §8.3, §11.1).
type Executor struct {
	kube      kubernetes.Interface
	providers ProviderGetter
	auditor   Auditor
	sched     *Scheduler
	logs      LogReader
	cfg       ExecutorConfig
}

func NewExecutor(kube kubernetes.Interface, providers ProviderGetter, auditor Auditor, sched *Scheduler, logs LogReader, cfg ExecutorConfig) *Executor {
	return &Executor{kube: kube, providers: providers, auditor: auditor, sched: sched, logs: logs, cfg: cfg.withDefaults()}
}

// RunRequest is one authorized command execution.
type RunRequest struct {
	// Actor names the caller the run is performed for. It is required and is recorded
	// on every entry except egress.revoked, which is gateway-initiated. M2b fills it
	// from gateway.CallerFrom(ctx).
	Actor        string
	Intent       string
	Stage        string
	SubjectHash  string
	AuthorizedBy *audit.Authorization
	Command      lastv1alpha1.Command
	Priority     Priority
}

// RunResult is the command's outcome. Result.ExitCode is -1 when it could not run.
type RunResult struct {
	RunID        string
	Result       runnerapi.Result
	CleanupError string
}

type commandStart struct {
	RunID     string               `json:"runId"`
	Image     string               `json:"image"`
	Command   lastv1alpha1.Command `json:"command"`
	Providers []string             `json:"providers,omitempty"`
	Egress    []string             `json:"egress,omitempty"`
}

type commandResult struct {
	RunID           string `json:"runId"`
	StartSeq        uint64 `json:"startSeq"`
	ExitCode        int    `json:"exitCode"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated"`
	StderrTruncated bool   `json:"stderrTruncated"`
	Error           string `json:"error,omitempty"`
	CleanupError    string `json:"cleanupError,omitempty"`
}

type credentialIssued struct {
	RunID string `json:"runId"`
	IssuedCredential
}

type egressGranted struct {
	RunID string   `json:"runId"`
	Rules []string `json:"rules"`
}

type runRef struct {
	RunID string `json:"runId"`
}

// prepared is a validated run: nothing has been audited or created yet.
type prepared struct {
	spec    runnerapi.Spec
	total   time.Duration
	rules   []EgressRule // includes the API server when a kubernetes provider is used
	plans   []CredentialPlan
	credCfg CredentialConfig
}

// launchHooks let the caller audit each credential issuance and the egress grant
// before it happens. Nil hooks skip per-step entries (compact probe audit).
type launchHooks struct {
	beforeCredential func(context.Context, CredentialPlan) error
	beforeEgress     func(context.Context, []EgressRule) error
}

// Run executes one command. Validation failures return before anything is audited
// or created; once command.start is durable, every path ends with command.result.
func (e *Executor) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if req.Actor == "" {
		return RunResult{}, errors.New("sandbox: run request has no actor")
	}
	p, err := e.prepare(ctx, []lastv1alpha1.Command{req.Command}, req.Command.Providers, req.Command.Egress)
	if err != nil {
		return RunResult{}, err
	}
	release, err := e.sched.Acquire(ctx, req.Priority)
	if err != nil {
		return RunResult{}, err
	}
	defer release()

	runID := e.cfg.NewRunID()
	out := RunResult{RunID: runID, Result: runnerapi.Result{ID: req.Command.ID, ExitCode: -1}}
	draft := func(typ string, payload any) audit.Draft {
		return audit.Draft{Type: typ, Actor: req.Actor, Intent: req.Intent, Stage: req.Stage,
			SubjectHash: req.SubjectHash, AuthorizedBy: req.AuthorizedBy, Payload: payload}
	}
	hooks := launchHooks{
		beforeCredential: func(ctx context.Context, cp CredentialPlan) error {
			_, err := e.auditor.Append(ctx, draft(audit.TypeCredentialIssued, credentialIssued{RunID: runID, IssuedCredential: cp.Issued}))
			return classifyAudit(err)
		},
		beforeEgress: func(ctx context.Context, rules []EgressRule) error {
			_, err := e.auditor.Append(ctx, draft(audit.TypeEgressGranted, egressGranted{RunID: runID, Rules: ruleStrings(rules)}))
			return classifyAudit(err)
		},
	}
	runCtx := ctx
	// The outcome entry must be written even when the request was cancelled, so the
	// audit calls ignore cancellation while the run itself still honors it.
	err = e.auditor.Do(context.WithoutCancel(ctx),
		draft(audit.TypeCommandStart, commandStart{RunID: runID, Image: e.cfg.Pod.Image, Command: req.Command,
			Providers: req.Command.Providers, Egress: ruleStrings(p.rules)}),
		func(context.Context) (any, error) {
			ctx := runCtx
			results, runErr := e.launch(ctx, runID, p, hooks)
			if runErr == nil && len(results) != 1 {
				runErr = fmt.Errorf("sandbox: expected 1 result, got %d", len(results))
			}
			if runErr == nil {
				out.Result = results[0]
			}
			onRevoked := func(ctx context.Context) error {
				rev := draft(audit.TypeEgressRevoked, runRef{RunID: runID})
				rev.Actor = GatewayActor // revocation is gateway-initiated, not the caller's act
				_, err := e.auditor.Append(ctx, rev)
				return err
			}
			if cerr := e.cleanup(ctx, runID, onRevoked); cerr != nil {
				out.CleanupError = cerr.Error()
			}
			return nil, runErr
		},
		func(start audit.Entry, _ any, actionErr error) audit.Draft {
			r := commandResult{RunID: runID, StartSeq: start.Seq, ExitCode: out.Result.ExitCode,
				Stdout: out.Result.Stdout, Stderr: out.Result.Stderr,
				StdoutTruncated: out.Result.StdoutTruncated, StderrTruncated: out.Result.StderrTruncated,
				Error: out.Result.Error, CleanupError: out.CleanupError}
			if actionErr != nil {
				r.ExitCode, r.Error = -1, actionErr.Error()
			}
			return draft(audit.TypeCommandResult, r)
		})
	return out, err
}

// prepare validates commands, egress and providers and resolves the API server
// for kubernetes providers. It only reads; nothing is created.
func (e *Executor) prepare(ctx context.Context, cmds []lastv1alpha1.Command, providers, egress []string) (prepared, error) {
	spec, total, err := RunnerSpec(cmds)
	if err != nil {
		return prepared{}, err
	}
	rules, err := parseRules(egress)
	if err != nil {
		return prepared{}, err
	}
	plans, err := PlanCredentials(ctx, e.providers, e.cfg.ProviderNamespace, providers)
	if err != nil {
		return prepared{}, err
	}
	// A kubernetes token must outlive the pod: its deadline plus the startup grace.
	RaiseTokenTTL(plans, total+e.cfg.DeadlineOverhead+e.cfg.StartupGrace)
	credCfg := CredentialConfig{SandboxNamespace: e.cfg.Pod.Namespace, ProviderNamespace: e.cfg.ProviderNamespace}
	if needsAPIServer(plans) {
		host, apiRules, err := APIServerEgress(ctx, e.kube)
		if err != nil {
			return prepared{}, err
		}
		credCfg.APIServerHost = host
		rules = append(rules, apiRules...)
	}
	return prepared{spec: spec, total: total, rules: rules, plans: plans, credCfg: credCfg}, nil
}

// launch issues credentials, grants egress, runs the pod and returns the runner's results.
// Credential values materialized for the run are redacted from the results (see
// redactor); this is best-effort: re-encoded or transformed values (reversed, hex,
// split) are not caught. The real boundary is that sandbox credentials are short-lived
// and that every command is audited.
func (e *Executor) launch(ctx context.Context, runID string, p prepared, hooks launchHooks) ([]runnerapi.Result, error) {
	ns := e.cfg.Pod.Namespace
	deadline := p.total + e.cfg.DeadlineOverhead
	// Every object records when the run is orphaned, so the reaper never guesses.
	expires := time.Now().Add(deadline + e.cfg.StartupGrace)
	for _, cp := range p.plans { // record each issuance before the credential is read or minted
		if hooks.beforeCredential != nil {
			if err := hooks.beforeCredential(ctx, cp); err != nil {
				return nil, err
			}
		}
	}
	creds, err := Materialize(ctx, e.kube, p.credCfg, runID, p.plans)
	if err != nil {
		return nil, err
	}
	if creds.Secret != nil {
		stampExpiry(&creds.Secret.ObjectMeta, expires)
		if _, err := e.kube.CoreV1().Secrets(ns).Create(ctx, creds.Secret, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("sandbox: create credential secret: %w", err)
		}
	}
	if np := EgressPolicy(ns, runID, p.rules); np != nil {
		stampExpiry(&np.ObjectMeta, expires)
		if hooks.beforeEgress != nil {
			if err := hooks.beforeEgress(ctx, p.rules); err != nil {
				return nil, err
			}
		}
		if _, err := e.kube.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("sandbox: create egress policy: %w", err)
		}
	}
	specSecret, err := SpecSecret(ns, runID, p.spec)
	if err != nil {
		return nil, err
	}
	stampExpiry(&specSecret.ObjectMeta, expires)
	if _, err := e.kube.CoreV1().Secrets(ns).Create(ctx, specSecret, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: create spec secret: %w", err)
	}
	pod := SandboxPod(e.cfg.Pod, runID, creds, deadline)
	stampExpiry(&pod.ObjectMeta, expires)
	if _, err := e.kube.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: create pod: %w", err)
	}
	results, err := e.await(ctx, pod.Name, deadline+e.cfg.StartupGrace)
	if err != nil {
		return nil, err
	}
	if creds.Secret != nil {
		r := newRedactor(creds.Secret.Data)
		for i := range results {
			results[i].Stdout = r.redact(results[i].Stdout)
			results[i].Stderr = r.redact(results[i].Stderr)
			results[i].Error = r.redact(results[i].Error)
		}
	}
	return results, nil
}

// failFastReasons are container waiting reasons that cannot progress without intervention.
var failFastReasons = []string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError"}

// await polls the pod until it finishes or limit passes, then parses its envelope.
func (e *Executor) await(ctx context.Context, name string, limit time.Duration) ([]runnerapi.Result, error) {
	ns := e.cfg.Pod.Namespace
	waitCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()
	var last *corev1.Pod
	for {
		pod, err := e.kube.CoreV1().Pods(ns).Get(waitCtx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			last = pod
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				return e.collect(ctx, pod)
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && slices.Contains(failFastReasons, w.Reason) {
					return nil, fmt.Errorf("sandbox: pod %s cannot start (%s)", name, describePod(pod))
				}
			}
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("sandbox: pod %s was deleted externally", name)
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, fmt.Errorf("sandbox: run cancelled while waiting for pod %s (%s): %w", name, describePod(last), ctx.Err())
			}
			return nil, fmt.Errorf("sandbox: pod %s did not finish within %s (%s)", name, limit, describePod(last))
		case <-ticker.C:
		}
	}
}

func (e *Executor) collect(ctx context.Context, pod *corev1.Pod) ([]runnerapi.Result, error) {
	lctx, cancel := context.WithTimeout(ctx, e.cfg.CleanupTimeout)
	defer cancel()
	log, err := e.logs(lctx, pod.Namespace, pod.Name)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read pod %s log (%s): %w", pod.Name, describePod(pod), err)
	}
	env, err := runnerapi.ParseEnvelope(log)
	if err != nil {
		return nil, fmt.Errorf("sandbox: pod %s finished (%s) but %w", pod.Name, describePod(pod), err)
	}
	return env.Results, nil
}

// cleanup deletes every object of the run, ignoring the caller's cancellation so
// credentials and egress grants never outlive a cancelled request. onRevoked (may
// be nil) records the revocation once a policy was actually deleted.
func (e *Executor) cleanup(ctx context.Context, runID string, onRevoked func(context.Context) error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cfg.CleanupTimeout)
	defer cancel()
	policies, err := e.deleteRun(cctx, LabelRun+"="+runID+","+LabelManagedBy+"="+ManagedByValue)
	if policies > 0 && onRevoked != nil {
		err = errors.Join(err, onRevoked(cctx))
	}
	return err
}

// deleteRun deletes the pods, secrets and network policies matching selector in the
// sandbox namespace and reports how many network policies it deleted.
func (e *Executor) deleteRun(ctx context.Context, selector string) (int, error) {
	ns := e.cfg.Pod.Namespace
	opts := metav1.ListOptions{LabelSelector: selector}
	zero := int64(0)
	var errs []error
	ignore := func(err error) error {
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if pods, err := e.kube.CoreV1().Pods(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, p := range pods.Items {
			errs = append(errs, ignore(e.kube.CoreV1().Pods(ns).Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})))
		}
	}
	if secrets, err := e.kube.CoreV1().Secrets(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, s := range secrets.Items {
			errs = append(errs, ignore(e.kube.CoreV1().Secrets(ns).Delete(ctx, s.Name, metav1.DeleteOptions{})))
		}
	}
	deleted := 0
	if nps, err := e.kube.NetworkingV1().NetworkPolicies(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, np := range nps.Items {
			if err := ignore(e.kube.NetworkingV1().NetworkPolicies(ns).Delete(ctx, np.Name, metav1.DeleteOptions{})); err != nil {
				errs = append(errs, err)
			} else {
				deleted++
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return deleted, fmt.Errorf("sandbox: cleanup %s: %w", selector, err)
	}
	return deleted, nil
}

func describePod(p *corev1.Pod) string {
	if p == nil {
		return "pod not found"
	}
	var desc strings.Builder
	desc.WriteString("phase " + string(p.Status.Phase))
	if p.Status.Reason != "" {
		desc.WriteString(", reason " + p.Status.Reason)
	}
	for _, cs := range p.Status.ContainerStatuses {
		switch {
		case cs.State.Terminated != nil:
			fmt.Fprintf(&desc, ", container %s terminated: %s (exit %d)", cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		case cs.State.Waiting != nil:
			fmt.Fprintf(&desc, ", container %s waiting: %s", cs.Name, cs.State.Waiting.Reason)
		}
	}
	return desc.String()
}

// classifyAudit marks an audit write failure as ErrAuditUnavailable.
func classifyAudit(err error) error {
	if err == nil || errors.Is(err, audit.ErrAuditUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", audit.ErrAuditUnavailable, err)
}

const redacted = "[REDACTED]"

// minRedactLen keeps short values from shredding the output.
const minRedactLen = 8

// redactor replaces credential values in command output.
type redactor struct{ needles []string }

func newRedactor(data map[string][]byte) redactor {
	seen := map[string]bool{}
	var needles []string
	add := func(n string) {
		if len(n) >= minRedactLen && !seen[n] {
			seen[n] = true
			needles = append(needles, n)
		}
	}
	for _, v := range data {
		add(string(v))
		add(strings.TrimSpace(string(v)))
		add(base64.StdEncoding.EncodeToString(v))
		add(base64.RawStdEncoding.EncodeToString(v))
	}
	slices.SortFunc(needles, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	return redactor{needles: needles}
}

func (r redactor) redact(s string) string {
	for _, n := range r.needles {
		s = strings.ReplaceAll(s, n, redacted)
	}
	return s
}

func parseRules(egress []string) ([]EgressRule, error) {
	rules := make([]EgressRule, 0, len(egress))
	for _, s := range egress {
		r, err := ParseEgress(s)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func ruleStrings(rules []EgressRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.String())
	}
	return out
}

func needsAPIServer(plans []CredentialPlan) bool {
	for _, p := range plans {
		if p.Provider.Spec.Type == lastv1alpha1.ProviderKubernetes {
			return true
		}
	}
	return false
}

func randomRunID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
