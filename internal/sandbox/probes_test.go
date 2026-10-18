package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	probeBin    = "kubectl"
	probeIntent = "team-x/web"
	probeImage  = "img:test"
	otherName   = "other"
)

func probeOf(id, assert string, egress ...string) lastv1alpha1.Probe {
	return lastv1alpha1.Probe{ID: id, Assert: assert, Command: lastv1alpha1.Command{
		Run: lastv1alpha1.CommandRun{Argv: []string{probeBin, "get", "deploy", "-o", "json"}}, Egress: egress,
		Timeout: &metav1.Duration{Duration: 10 * time.Millisecond}}}
}

func countPods(xs []string) int {
	n := 0
	for _, v := range xs {
		if v == podsResource {
			n++
		}
	}
	return n
}

func TestRunProbesGroupsByProvidersAndEgressWithCompactAudit(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	logs := podLogs(kube, map[string]runnerapi.Result{
		"replicas": {ID: "replicas", Stdout: `{"spec":{"replicas":2}}`},
		"ready":    {ID: "ready", Stdout: `{"status":{"readyReplicas":1}}`},
		"remote":   {ID: "remote", Stdout: "ok"},
	})
	outs, err := newProbeExec(kube, aud, logs).RunProbes(context.Background(), sandbox.ProbeRunRequest{Actor: testActor, Intent: probeIntent, Probes: []lastv1alpha1.Probe{
		probeOf("replicas", "result.json.spec.replicas == 2"),
		probeOf("ready", "result.json.status.readyReplicas == 2"),
		probeOf("remote", `result.stdout == "ok"`, testEgress),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 3 || !outs[0].Passed || outs[1].Passed || outs[1].Error != "" || !outs[2].Passed {
		t.Fatalf("outcomes = %+v", outs)
	}
	if n := countPods(*created); n != 2 {
		t.Fatalf("created %d pods, want 2 (one per provider/egress group)", n)
	}
	want := []string{audit.TypeProbeStart, audit.TypeProbeResult, audit.TypeProbeStart, audit.TypeProbeResult}
	if got := entryTypes(entries(t, sink)); !slices.Equal(got, want) {
		t.Fatalf("audit types = %v, want %v (no per-step entries for probes)", got, want)
	}
	assertCleaned(t, kube)
}

func TestRunProbesBadAssertionIsAFailedProbeNotAnError(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded)
	aud, _ := newAudit(t)
	outs, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "p", Stdout: "not json"})).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{probeOf("p", "result.json.x == 1")}})
	if err != nil || outs[0].Passed || outs[0].Error == "" {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
}

func TestRunProbesInfrastructureFailureFailsEveryProbeInTheGroup(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodPending)
	aud, sink := newAudit(t)
	outs, err := newExec(kube, aud, envelopeLogs()).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{probeOf("a", "true"), probeOf("b", "true")}})
	var res struct{ Results []probeResultJSON }
	if es := entries(t, sink); len(es) > 0 {
		_ = json.Unmarshal(es[len(es)-1].Payload, &res)
	}
	if len(res.Results) != 2 || res.Results[0].ID != "a" || res.Results[1].ID != "b" || res.Results[0].Passed || res.Results[1].Error == "" {
		t.Fatalf("probe.result must hold one failed outcome per probe: %+v", res)
	}
	if err == nil || len(outs) != 2 || outs[0].Passed || outs[1].Passed || !strings.Contains(outs[1].Error, "did not finish") {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
	es := entries(t, sink)
	if last := es[len(es)-1]; last.Type != audit.TypeProbeResult || !strings.Contains(string(last.Payload), "did not finish") {
		t.Fatalf("last entry = %s %s", last.Type, last.Payload)
	}
	assertCleaned(t, kube)
}

func TestRunProbesRejectsDuplicateIDs(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, _ := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs()).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{probeOf("a", "true"), probeOf("a", "true")}})
	if err == nil || len(*created) != 0 {
		t.Fatalf("err=%v created=%v (nothing may be created)", err, *created)
	}
}

func TestRunProbesFailsClosedWithoutAudit(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	sink.SetFailPuts(true)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "a"})).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{probeOf("a", "true")}})
	if !errors.Is(err, audit.ErrAuditUnavailable) || len(*created) != 0 {
		t.Fatalf("err=%v created=%v", err, *created)
	}
}

type probeResultJSON struct {
	ID     string `json:"id"`
	Passed bool   `json:"passed"`
	Error  string `json:"error"`
	Stdout string `json:"stdout"`
}

type probeStartJSON struct {
	RunID  string `json:"runId"`
	Probes []struct {
		ID string `json:"id"`
	} `json:"probes"`
}

type probeResultPayloadJSON struct {
	RunID         string            `json:"runId"`
	StartSeq      uint64            `json:"startSeq"`
	Results       []probeResultJSON `json:"results"`
	Error         string            `json:"error"`
	EgressRevoked bool              `json:"egressRevoked"`
}

// newProbeExec is like newExec but gives every run its own run ID, so each group's pod
// has its own name.
func newProbeExec(kube *fake.Clientset, aud sandbox.Auditor, logs sandbox.LogReader) *sandbox.Executor {
	ps := []*lastv1alpha1.Provider{envProvider(), otherProvider()}
	var n atomic.Int64
	return sandbox.NewExecutor(kube, providers(ps...), aud, sandbox.NewScheduler(2), logs,
		sandbox.ExecutorConfig{
			Pod:               sandbox.PodConfig{Namespace: sandboxNS, Image: probeImage},
			ProviderNamespace: gatewayNS,
			PollInterval:      time.Millisecond,
			DeadlineOverhead:  time.Millisecond,
			StartupGrace:      50 * time.Millisecond,
			CleanupTimeout:    time.Second,
			NewRunID:          func() string { return fmt.Sprintf("run%d", n.Add(1)) },
		})
}

func otherProvider() *lastv1alpha1.Provider {
	return &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: otherName}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderEnvSecret, SecretRef: &lastv1alpha1.LocalRef{Name: demoCreds},
		Env: map[string]string{"OTHER_TOKEN": tokenKey},
	}}
}

// podLogs returns, for each pod, only the results of the probes in that pod's own spec,
// so a mis-grouped run is visible as a missing result.
func podLogs(kube *fake.Clientset, byID map[string]runnerapi.Result) sandbox.LogReader {
	return func(ctx context.Context, ns, pod string) ([]byte, error) {
		sec, err := kube.CoreV1().Secrets(ns).Get(ctx, pod+"-spec", metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		var spec runnerapi.Spec
		if err := json.Unmarshal(sec.Data["spec.json"], &spec); err != nil {
			return nil, err
		}
		var results []runnerapi.Result
		for _, c := range spec.Commands {
			if r, ok := byID[c.ID]; ok {
				results = append(results, r)
			}
		}
		return envelopeLogs(results...)(ctx, ns, pod)
	}
}

func probeEntries(t *testing.T, sink *audittest.MemSink) (starts []audit.Entry, results []audit.Entry) {
	t.Helper()
	for _, e := range entries(t, sink) {
		switch e.Type {
		case audit.TypeProbeStart:
			starts = append(starts, e)
		case audit.TypeProbeResult:
			results = append(results, e)
		}
	}
	return starts, results
}

func decode[T any](t *testing.T, e audit.Entry) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRunProbesInterleavedGroupsKeepInputOrderAndPairStartWithResult(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	logs := podLogs(kube, map[string]runnerapi.Result{
		"a": {ID: "a", Stdout: "ok"}, "b": {ID: "b", Stdout: "ok"}, "c": {ID: "c", Stdout: "ok"},
	})
	probes := []lastv1alpha1.Probe{
		probeOf("a", `result.stdout == "ok"`, testEgress),
		probeOf("b", `result.stdout == "ok"`),
		probeOf("c", `result.stdout == "ok"`, testEgress),
	}
	outs, err := newProbeExec(kube, aud, logs).RunProbes(context.Background(), sandbox.ProbeRunRequest{Actor: testActor, Probes: probes})
	if err != nil {
		t.Fatal(err)
	}
	if n := countPods(*created); n != 2 {
		t.Fatalf("created %d pods, want 2", n)
	}
	for i, id := range []string{"a", "b", "c"} {
		if outs[i].ID != id || !outs[i].Passed {
			t.Fatalf("outcomes out of input order or failed: %+v", outs)
		}
	}
	starts, results := probeEntries(t, sink)
	if len(starts) != 2 || len(results) != 2 {
		t.Fatalf("%d starts, %d results", len(starts), len(results))
	}
	wantIDs := [][]string{{"a", "c"}, {"b"}}
	wantRevoked := []bool{true, false}
	for i := range starts {
		st := decode[probeStartJSON](t, starts[i])
		var ids []string
		for _, p := range st.Probes {
			ids = append(ids, p.ID)
		}
		if !slices.Equal(ids, wantIDs[i]) {
			t.Fatalf("start %d lists %v, want %v", i, ids, wantIDs[i])
		}
		res := decode[probeResultPayloadJSON](t, results[i])
		if res.RunID != st.RunID || res.StartSeq != starts[i].Seq {
			t.Fatalf("result %d (run %s, startSeq %d) does not pair with start (run %s, seq %d)", i, res.RunID, res.StartSeq, st.RunID, starts[i].Seq)
		}
		var rids []string
		for _, r := range res.Results {
			rids = append(rids, r.ID)
		}
		if !slices.Equal(rids, wantIDs[i]) {
			t.Fatalf("result %d holds %v, want %v", i, rids, wantIDs[i])
		}
		if res.EgressRevoked != wantRevoked[i] {
			t.Fatalf("result %d egressRevoked = %v, want %v", i, res.EgressRevoked, wantRevoked[i])
		}
	}
	assertCleaned(t, kube)
}

func TestRunProbesGroupsBySetEquality(t *testing.T) {
	cases := []struct {
		name string
		a, b func(lastv1alpha1.Probe) lastv1alpha1.Probe
	}{
		{"providers in a different order",
			func(p lastv1alpha1.Probe) lastv1alpha1.Probe {
				p.Command.Providers = []string{demoProvider, otherName}
				return p
			},
			func(p lastv1alpha1.Probe) lastv1alpha1.Probe {
				p.Command.Providers = []string{otherName, demoProvider}
				return p
			}},
		{"duplicate providers",
			func(p lastv1alpha1.Probe) lastv1alpha1.Probe {
				p.Command.Providers = []string{demoProvider, demoProvider}
				return p
			},
			func(p lastv1alpha1.Probe) lastv1alpha1.Probe { p.Command.Providers = []string{demoProvider}; return p }},
		{"egress in a different order",
			func(p lastv1alpha1.Probe) lastv1alpha1.Probe {
				p.Command.Egress = []string{testEgress, "10.1.2.4:443"}
				return p
			},
			func(p lastv1alpha1.Probe) lastv1alpha1.Probe {
				p.Command.Egress = []string{"10.1.2.4:443", testEgress}
				return p
			}},
		{"duplicate egress",
			func(p lastv1alpha1.Probe) lastv1alpha1.Probe {
				p.Command.Egress = []string{testEgress, testEgress}
				return p
			},
			func(p lastv1alpha1.Probe) lastv1alpha1.Probe { p.Command.Egress = []string{testEgress}; return p }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kube, created := fakeCluster(corev1.PodSucceeded, demoSecret())
			aud, _ := newAudit(t)
			logs := podLogs(kube, map[string]runnerapi.Result{"a": {ID: "a"}, "b": {ID: "b"}})
			_, err := newProbeExec(kube, aud, logs).RunProbes(context.Background(), sandbox.ProbeRunRequest{Actor: testActor,
				Probes: []lastv1alpha1.Probe{c.a(probeOf("a", "true")), c.b(probeOf("b", "true"))}})
			if err != nil {
				t.Fatal(err)
			}
			if n := countPods(*created); n != 1 {
				t.Fatalf("created %d pods, want 1", n)
			}
		})
	}
}

func TestRunProbesRedactsIssuedCredentialsInAProviderGroup(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	logs := podLogs(kube, map[string]runnerapi.Result{"p": {ID: "p", Stdout: "token=" + secretValue}})
	p := probeOf("p", `result.stdout.contains("REDACTED")`)
	p.Command.Providers = []string{demoProvider}
	outs, err := newProbeExec(kube, aud, logs).RunProbes(context.Background(), sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{p}})
	if err != nil || !outs[0].Passed {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
	for _, e := range entries(t, sink) {
		if strings.Contains(string(e.Payload), secretValue) {
			t.Fatalf("entry %s leaks the credential: %s", e.Type, e.Payload)
		}
	}
	_, results := probeEntries(t, sink)
	if got := decode[probeResultPayloadJSON](t, results[0]).Results[0].Stdout; !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("probe.result stdout = %q", got)
	}
}

func TestRunProbesMidRunFailureDoesNotStopLaterGroups(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded)
	var pods atomic.Int64
	kube.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if pods.Add(1) == 1 {
			return true, nil, errors.New("quota exceeded")
		}
		return false, nil, nil
	})
	aud, sink := newAudit(t)
	logs := podLogs(kube, map[string]runnerapi.Result{"a": {ID: "a", Stdout: "ok"}, "b": {ID: "b", Stdout: "ok"}})
	outs, err := newProbeExec(kube, aud, logs).RunProbes(context.Background(), sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{
		probeOf("a", "true", testEgress), probeOf("b", `result.stdout == "ok"`)}})
	if err == nil || len(outs) != 2 {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
	if outs[0].Passed || !strings.Contains(outs[0].Error, "quota exceeded") || !outs[1].Passed {
		t.Fatalf("outs = %+v", outs)
	}
	starts, results := probeEntries(t, sink)
	if len(starts) != 2 || len(results) != 2 {
		t.Fatalf("%d starts, %d results", len(starts), len(results))
	}
	r0 := decode[probeResultPayloadJSON](t, results[0])
	if len(r0.Results) != 1 || r0.Results[0].ID != "a" || r0.Results[0].Passed || !strings.Contains(r0.Results[0].Error, "quota exceeded") {
		t.Fatalf("group 1 result = %+v", r0)
	}
	if !r0.EgressRevoked {
		t.Fatal("group 1 created its egress policy, so egressRevoked must be true")
	}
	assertCleaned(t, kube)
}

func TestRunProbesCancelledRunStillPairsEveryStartWithAResult(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodPending)
	aud, sink := newAudit(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	p1, p2 := probeOf("a", "true", testEgress), probeOf("b", "true")
	p1.Command.Timeout = &metav1.Duration{Duration: time.Minute}
	p2.Command.Timeout = &metav1.Duration{Duration: time.Minute}
	outs, err := newProbeExec(kube, aud, envelopeLogs()).RunProbes(ctx, sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{p1, p2}})
	if !errors.Is(err, context.Canceled) || outs[0].Passed || outs[1].Passed {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
	starts, results := probeEntries(t, sink)
	if len(starts) == 0 || len(starts) != len(results) {
		t.Fatalf("%d starts, %d results", len(starts), len(results))
	}
	for i := range starts {
		if decode[probeResultPayloadJSON](t, results[i]).StartSeq != starts[i].Seq {
			t.Fatalf("result %d does not pair with start %d", i, i)
		}
	}
	assertCleaned(t, kube)
}

func TestRunProbesAuditsStartBeforeAnyCredentialReadOrCreate(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, _, l := newRecAudit(t, 0)
	recordKube(kube, l)
	p := probeOf("p", "true", testEgress)
	p.Command.Providers = []string{demoProvider}
	if _, err := newProbeExec(kube, aud, podLogs(kube, map[string]runnerapi.Result{"p": {ID: "p"}})).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{p}}); err != nil {
		t.Fatal(err)
	}
	ev := l.all()
	start := slices.Index(ev, "audit:"+audit.TypeProbeStart)
	if start < 0 {
		t.Fatalf("no probe.start: %v", ev)
	}
	for i, e := range ev {
		if i < start && (strings.HasPrefix(e, "kube:create") || strings.HasPrefix(e, "kube:get:secrets")) {
			t.Fatalf("%s happened before probe.start: %v", e, ev)
		}
	}
	if ev[len(ev)-1] != "audit:"+audit.TypeProbeResult {
		t.Fatalf("probe.result must be last: %v", ev)
	}
}

func TestRunProbesTruncatesLongAssertionErrors(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	assertion := `result.stdout == "` + strings.Repeat("é", 3000) + `" && nope`
	outs, err := newProbeExec(kube, aud, podLogs(kube, map[string]runnerapi.Result{"p": {ID: "p"}})).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{probeOf("p", assertion)}})
	if err != nil || outs[0].Passed || !strings.HasSuffix(outs[0].Error, "…(truncated)") || len(outs[0].Error) > 1024+len("…(truncated)") {
		t.Fatalf("err=%v outs=%+v", err, outs)
	}
	if !utf8.ValidString(outs[0].Error) {
		t.Fatal("truncated error is not valid UTF-8")
	}
	_, results := probeEntries(t, sink)
	if got := decode[probeResultPayloadJSON](t, results[0]).Results[0].Error; got != outs[0].Error {
		t.Fatalf("audited error differs from the outcome error")
	}
}

const (
	imgB         = "reg/img@sha256:b"
	imgC         = "reg/img@sha256:c"
	podsResource = "pods"
)

func TestRunProbesGroupsByImageAndRunsEachGroupOnItsImage(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	logs := podLogs(kube, map[string]runnerapi.Result{"a": {ID: "a"}, "b": {ID: "b"}, "c": {ID: "c"}, "d": {ID: "d"}})
	withInstall := func(p lastv1alpha1.Probe, argv ...string) lastv1alpha1.Probe {
		p.Command.Image = &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: argv}}}
		return p
	}
	a, b := probeOf("a", "true"), withInstall(probeOf("b", "true"), "apt-get", "install", "jq")
	c, d := withInstall(probeOf("c", "true"), "apt-get", "install", "yq"), withInstall(probeOf("d", "true"), "apt-get", "install", "jq")
	if sandbox.ImageKey(a.Command) != "" || sandbox.ImageKey(b.Command) == "" ||
		sandbox.ImageKey(b.Command) != sandbox.ImageKey(d.Command) || sandbox.ImageKey(b.Command) == sandbox.ImageKey(c.Command) {
		t.Fatalf("ImageKey: a=%q b=%q c=%q d=%q", sandbox.ImageKey(a.Command), sandbox.ImageKey(b.Command),
			sandbox.ImageKey(c.Command), sandbox.ImageKey(d.Command))
	}
	outs, err := newProbeExec(kube, aud, logs).RunProbes(context.Background(), sandbox.ProbeRunRequest{Actor: testActor,
		Intent: probeIntent, Probes: []lastv1alpha1.Probe{a, b, c, d},
		Images: map[string]string{sandbox.ImageKey(b.Command): imgB, sandbox.ImageKey(c.Command): imgC}})
	if err != nil || len(outs) != 4 || outs[0].ID != "a" || outs[3].ID != "d" {
		t.Fatalf("outs = %+v, err = %v", outs, err)
	}
	if n := countPods(*created); n != 3 {
		t.Fatalf("created %d pods, want 3", n)
	}
	starts, _ := probeEntries(t, sink)
	got := map[string]string{}
	for _, s := range starts {
		var p struct {
			Image  string                `json:"image"`
			Probes []struct{ ID string } `json:"probes"`
		}
		if err := json.Unmarshal(s.Payload, &p); err != nil {
			t.Fatal(err)
		}
		got[p.Probes[0].ID] = p.Image
	}
	want := map[string]string{"a": probeImage, "b": imgB, "c": imgC}
	if len(got) != 3 || got["a"] != want["a"] || got["b"] != want["b"] || got["c"] != want["c"] {
		t.Fatalf("probe.start images = %v, want %v", got, want)
	}
	var podImages []string
	for _, act := range kube.Actions() {
		if ca, ok := act.(k8stesting.CreateAction); ok && act.GetResource().Resource == "pods" {
			podImages = append(podImages, ca.GetObject().(*corev1.Pod).Spec.Containers[0].Image)
		}
	}
	slices.Sort(podImages)
	if !slices.Equal(podImages, []string{probeImage, imgB, imgC}) {
		t.Fatalf("pod images = %v", podImages)
	}
}
