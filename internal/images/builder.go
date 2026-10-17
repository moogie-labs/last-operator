package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	// ImagesRepo is the registry repository built images are pushed to.
	ImagesRepo = "last/images"

	// buildWaitSlack keeps the wait limit above the pod's activeDeadlineSeconds
	// (Timeout + buildDeadlineSlack), so Kubernetes ends a stuck pod first.
	buildWaitSlack = 5 * time.Minute
	buildPullGrace = 60 * time.Second
	cleanupTimeout = 30 * time.Second
	redactedMark   = "[REDACTED]"
	buildCommandID = "build"
	metadataDigest = "containerimage.digest"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// BuildOutcome is the result of one build.
type BuildOutcome struct {
	Digest       string        // "sha256:…", set on success
	Log          string        // the build's stderr, redacted, at most runnerapi.MaxOutput
	LogTruncated bool          //
	Duration     time.Duration //
	Err          error         // build failure (non-zero exit, timeout, no digest); nil on success
}

// Builder runs build pods.
type Builder struct {
	kube     kubernetes.Interface
	logs     sandbox.LogReader
	registry *Client
	cfg      BuildPodConfig
	poll     time.Duration
	now      func() time.Time
}

// NewBuilder returns a Builder. registry is used for anonymous manifest HEADs.
func NewBuilder(kube kubernetes.Interface, logs sandbox.LogReader, registry *Client, cfg BuildPodConfig) *Builder {
	return &Builder{kube: kube, logs: logs, registry: registry, cfg: cfg, poll: time.Second, now: time.Now}
}

// Build creates the objects, waits for the pod, reads the runner envelope and confirms the
// digest with Head; it always deletes the build's objects. A returned error means the build
// could not be attempted or its outcome is unknown (Kubernetes or registry errors);
// BuildOutcome.Err means the build itself failed, including a pod that ended without a
// result envelope.
func (b *Builder) Build(ctx context.Context, s BuildSpec) (BuildOutcome, error) {
	start := b.now()
	wait := b.cfg.Timeout + buildWaitSlack
	// expires covers the whole wait, so a live build is never mistaken for an orphan.
	secret, np, pod, err := BuildObjects(b.cfg, s, start.Add(wait+cleanupTimeout))
	if err != nil {
		return BuildOutcome{}, err
	}
	defer b.cleanup(ctx, s.ID)
	ns := b.cfg.Namespace
	if _, err := b.kube.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return BuildOutcome{}, fmt.Errorf("images: create build spec secret: %w", err)
	}
	if _, err := b.kube.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{}); err != nil {
		return BuildOutcome{}, fmt.Errorf("images: create build network policy: %w", err)
	}
	if _, err := b.kube.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return BuildOutcome{}, fmt.Errorf("images: create build pod: %w", err)
	}
	done, err := sandbox.WaitForPod(ctx, b.kube, ns, pod.Name, wait, b.poll, buildPullGrace)
	if err != nil {
		return BuildOutcome{}, fmt.Errorf("images: build %s: %w", s.ID, err)
	}

	r := newBuildRedactor(s.PushUser, s.PushPassword)
	out := BuildOutcome{}
	fail := func(format string, args ...any) (BuildOutcome, error) {
		out.Err = errors.New(r.redact(fmt.Sprintf(format, args...)))
		out.Duration = b.now().Sub(start)
		return out, nil
	}

	lctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	raw, err := b.logs(lctx, ns, pod.Name)
	if err != nil {
		if done.Status.Phase == corev1.PodFailed {
			return fail("build pod failed (%s) and its log could not be read", describe(done))
		}
		return BuildOutcome{}, fmt.Errorf("images: read build %s log: %w", s.ID, err)
	}
	env, err := runnerapi.ParseEnvelope(raw)
	if err != nil {
		return fail("build pod ended without a result (%s)", describe(done))
	}
	idx := slices.IndexFunc(env.Results, func(res runnerapi.Result) bool { return res.ID == buildCommandID })
	if idx < 0 {
		return fail("build pod reported no build result (%s)", describe(done))
	}
	res := env.Results[idx]
	out.Log = r.redact(res.Stderr)
	if len(out.Log) > runnerapi.MaxOutput {
		out.Log = out.Log[len(out.Log)-runnerapi.MaxOutput:]
		out.LogTruncated = true
	}
	out.LogTruncated = out.LogTruncated || res.StderrTruncated
	switch {
	case res.ExitCode != 0 && res.Error != "":
		return fail("build did not complete: %s", res.Error)
	case res.ExitCode != 0:
		return fail("build exited %d", res.ExitCode)
	case res.Error != "":
		return fail("build reported an error: %s", res.Error)
	}

	var md map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &md); err != nil {
		return fail("build metadata is not JSON")
	}
	digest, _ := md[metadataDigest].(string)
	if !digestPattern.MatchString(digest) {
		return fail("build metadata has no valid %s", metadataDigest)
	}
	found, err := b.registry.Head(ctx, ImagesRepo, digest)
	if err != nil {
		return BuildOutcome{}, fmt.Errorf("images: confirm built digest %s: %w", digest, err)
	}
	if !found {
		return fail("built digest %s is not in the registry", digest)
	}
	out.Digest = digest
	out.Duration = b.now().Sub(start)
	return out, nil
}

// cleanup deletes every object of the build, ignoring the caller's cancellation so the
// push credential never outlives a cancelled request.
func (b *Builder) cleanup(ctx context.Context, id string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_ = b.deleteBuilds(cctx, LabelBuild+"="+id)
}

func (b *Builder) deleteBuilds(ctx context.Context, selector string) error {
	ns := b.cfg.Namespace
	opts := metav1.ListOptions{LabelSelector: selector}
	zero := int64(0)
	var errs []error
	ignore := func(err error) {
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	if pods, err := b.kube.CoreV1().Pods(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, p := range pods.Items {
			ignore(b.kube.CoreV1().Pods(ns).Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}))
		}
	}
	if secrets, err := b.kube.CoreV1().Secrets(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, s := range secrets.Items {
			ignore(b.kube.CoreV1().Secrets(ns).Delete(ctx, s.Name, metav1.DeleteOptions{}))
		}
	}
	if nps, err := b.kube.NetworkingV1().NetworkPolicies(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, np := range nps.Items {
			ignore(b.kube.NetworkingV1().NetworkPolicies(ns).Delete(ctx, np.Name, metav1.DeleteOptions{}))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("images: cleanup %s: %w", selector, err)
	}
	return nil
}

// ReapOrphans deletes every object labelled LabelBuild in the namespace whose expiry passed
// (or all of them when all is true, as at startup). A build is reaped once the latest
// expiry among its objects has passed; an object without a parsable expiry counts from
// its creation time. It returns the number of builds reaped.
func (b *Builder) ReapOrphans(ctx context.Context, all bool) (int, error) {
	ns := b.cfg.Namespace
	opts := metav1.ListOptions{LabelSelector: LabelBuild}
	due := map[string]time.Time{}
	record := func(m metav1.ObjectMeta) {
		id := m.Labels[LabelBuild]
		t := m.CreationTimestamp.Time
		if secs, err := strconv.ParseInt(m.Labels[sandbox.LabelExpiresAt], 10, 64); err == nil {
			t = time.Unix(secs, 0)
		}
		if cur, ok := due[id]; !ok || t.After(cur) {
			due[id] = t
		}
	}
	pods, err := b.kube.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("images: list build pods: %w", err)
	}
	for _, p := range pods.Items {
		record(p.ObjectMeta)
	}
	secrets, err := b.kube.CoreV1().Secrets(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("images: list build secrets: %w", err)
	}
	for _, s := range secrets.Items {
		record(s.ObjectMeta)
	}
	nps, err := b.kube.NetworkingV1().NetworkPolicies(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("images: list build network policies: %w", err)
	}
	for _, np := range nps.Items {
		record(np.ObjectMeta)
	}
	now := b.now()
	var ids []string
	for id, t := range due {
		if id != "" && (all || t.Before(now)) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var errs []error
	reaped := 0
	for _, id := range ids {
		if err := b.deleteBuilds(ctx, LabelBuild+"="+id); err != nil {
			errs = append(errs, err)
		} else {
			reaped++
		}
	}
	return reaped, errors.Join(errs...)
}

// describe summarises a finished build pod for failure messages.
func describe(p *corev1.Pod) string {
	var sb strings.Builder
	sb.WriteString("phase " + string(p.Status.Phase))
	if p.Status.Reason != "" {
		sb.WriteString(", reason " + p.Status.Reason)
	}
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			fmt.Fprintf(&sb, ", container %s terminated: %s (exit %d)", cs.Name, t.Reason, t.ExitCode)
		}
	}
	return sb.String()
}

// buildRedactor replaces the push credential, in the raw and base64 forms a tool might
// print, with [REDACTED].
type buildRedactor struct{ needles []string }

func newBuildRedactor(user, password string) buildRedactor {
	var needles []string
	if password != "" {
		pair := []byte(user + ":" + password)
		for _, v := range [][]byte{pair, []byte(password)} {
			needles = append(needles, base64.StdEncoding.EncodeToString(v), base64.RawStdEncoding.EncodeToString(v))
		}
		needles = append(needles, password)
	}
	// Longest first, so a value contained in another is not redacted piecemeal.
	slices.SortFunc(needles, func(a, b string) int { return len(b) - len(a) })
	return buildRedactor{needles: needles}
}

func (r buildRedactor) redact(s string) string {
	for _, n := range r.needles {
		s = strings.ReplaceAll(s, n, redactedMark)
	}
	return s
}
