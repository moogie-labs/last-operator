package sandbox_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	lastSandboxNS = "last-sandbox"
	testRunID     = "run1"
	testImage     = "img:tag"
	workDir       = "/work"
	dropAll       = "ALL"
)

func TestRunnerSpecConvertsCommands(t *testing.T) {
	spec, total, err := sandbox.RunnerSpec([]lastv1alpha1.Command{
		{ID: "a", Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", podsResource}}, Timeout: &metav1.Duration{Duration: time.Minute}},
		{Run: lastv1alpha1.CommandRun{Script: "echo hi"}, Files: map[string]string{"/work/x": "y"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != runnerapi.Version || len(spec.Commands) != 2 {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Commands[0].ID != "a" || spec.Commands[0].TimeoutSeconds != 60 || spec.Commands[0].Argv[0] != "kubectl" {
		t.Fatalf("cmd0 = %+v", spec.Commands[0])
	}
	if spec.Commands[1].ID != "cmd-1" || spec.Commands[1].TimeoutSeconds != 300 || spec.Commands[1].Files["/work/x"] != "y" {
		t.Fatalf("cmd1 = %+v", spec.Commands[1])
	}
	if total != 6*time.Minute {
		t.Fatalf("total timeout = %s", total)
	}
}

func TestRunnerSpecAcceptsAnImage(t *testing.T) {
	spec, _, err := sandbox.RunnerSpec([]lastv1alpha1.Command{{Run: lastv1alpha1.CommandRun{Argv: []string{"aws"}},
		Image: &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "awscli"}}}}}})
	if err != nil || len(spec.Commands) != 1 || spec.Commands[0].Argv[0] != "aws" {
		t.Fatalf("spec = %+v, err = %v", spec, err)
	}
}

func TestRunnerSpecRejectsBadCommands(t *testing.T) {
	for _, c := range []lastv1alpha1.Command{
		{Run: lastv1alpha1.CommandRun{}},
		{Run: lastv1alpha1.CommandRun{Argv: []string{"a"}, Script: "b"}},
		{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}, Files: map[string]string{"/etc/x": "y"}},
	} {
		if _, _, err := sandbox.RunnerSpec([]lastv1alpha1.Command{c}); err == nil {
			t.Errorf("accepted invalid command %+v", c)
		}
	}
	if _, _, err := sandbox.RunnerSpec(nil); err == nil {
		t.Error("accepted an empty command list")
	}
}

func TestSpecSecretHoldsTheSpec(t *testing.T) {
	spec := runnerapi.Spec{Version: 1, Commands: []runnerapi.Command{{ID: "a", Argv: []string{"true"}, TimeoutSeconds: 5}}}
	s, err := sandbox.SpecSecret(lastSandboxNS, testRunID, spec)
	if err != nil {
		t.Fatal(err)
	}
	var back runnerapi.Spec
	if err := json.Unmarshal(s.Data["spec.json"], &back); err != nil || back.Commands[0].ID != "a" {
		t.Fatalf("spec.json = %s (%v)", s.Data["spec.json"], err)
	}
	if s.Name != "last-run-"+testRunID+"-spec" || s.Namespace != lastSandboxNS || s.Labels[sandbox.LabelRun] != testRunID {
		t.Fatalf("secret meta = %+v", s.ObjectMeta)
	}
}

func TestSandboxPodIsLockedDown(t *testing.T) {
	creds := sandbox.Materialized{Env: []corev1.EnvVar{{Name: "DEMO", Value: "x"}}}
	pod := sandbox.SandboxPod(sandbox.PodConfig{Namespace: lastSandboxNS, Image: testImage, ImagePullPolicy: corev1.PullIfNotPresent},
		testRunID, creds, 7*time.Minute)
	ps := pod.Spec
	if ps.RestartPolicy != corev1.RestartPolicyNever || ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken ||
		ps.EnableServiceLinks == nil || *ps.EnableServiceLinks || *ps.ActiveDeadlineSeconds != 420 {
		t.Fatalf("pod spec = %+v", ps)
	}
	if !*ps.SecurityContext.RunAsNonRoot || *ps.SecurityContext.RunAsUser != 65532 || ps.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security = %+v", ps.SecurityContext)
	}
	c := ps.Containers[0]
	if c.Name != "runner" || c.Image != testImage || *c.SecurityContext.AllowPrivilegeEscalation || !*c.SecurityContext.ReadOnlyRootFilesystem ||
		len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != dropAll {
		t.Fatalf("container = %+v", c)
	}
	mounts := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = true
	}
	if !mounts[workDir] || !mounts["/tmp"] || !mounts["/last/spec"] {
		t.Fatalf("mounts = %+v", c.VolumeMounts)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["HOME"] != workDir || env["DEMO"] != "x" {
		t.Fatalf("env = %+v", c.Env)
	}
	if pod.Labels[sandbox.LabelRun] != testRunID || pod.Labels[sandbox.LabelName] != sandbox.NameValue || pod.Name != "last-run-"+testRunID {
		t.Fatalf("meta = %+v", pod.ObjectMeta)
	}
}

func TestSandboxPodStartsTheRunnerFromTheBase(t *testing.T) {
	const built, base = "registry/last/images@sha256:built", "base@sha256:pinned"
	pod := sandbox.SandboxPod(sandbox.PodConfig{Namespace: lastSandboxNS, Image: built, RunnerImage: base,
		ImagePullPolicy: corev1.PullIfNotPresent}, testRunID, sandbox.Materialized{}, time.Minute)
	ps := pod.Spec
	if len(ps.InitContainers) != 1 {
		t.Fatalf("init containers = %+v", ps.InitContainers)
	}
	init := ps.InitContainers[0]
	if init.Image != base || init.ImagePullPolicy != corev1.PullIfNotPresent ||
		!slices.Equal(init.Command, []string{"cp", "/last/sandbox-runner", "/last-bin/sandbox-runner"}) {
		t.Fatalf("init container = %+v", init)
	}
	checkRunnerCopySecurity(t, init.SecurityContext)
	if init.Resources.Limits.Memory().IsZero() || init.Resources.Requests.Cpu().IsZero() {
		t.Fatalf("init resources = %+v", init.Resources)
	}
	if len(init.VolumeMounts) != 1 || init.VolumeMounts[0].MountPath != "/last-bin" || init.VolumeMounts[0].ReadOnly {
		t.Fatalf("init mounts = %+v", init.VolumeMounts)
	}
	c := ps.Containers[0]
	if c.Image != built || !slices.Equal(c.Command, []string{"/last-bin/sandbox-runner"}) || len(c.Args) != 0 {
		t.Fatalf("runner container image %q command %v args %v", c.Image, c.Command, c.Args)
	}
	i := slices.IndexFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == "/last-bin" })
	if i < 0 || !c.VolumeMounts[i].ReadOnly || c.VolumeMounts[i].Name != init.VolumeMounts[0].Name {
		t.Fatalf("runner mounts = %+v", c.VolumeMounts)
	}
	v := slices.IndexFunc(ps.Volumes, func(v corev1.Volume) bool { return v.Name == init.VolumeMounts[0].Name })
	if v < 0 || ps.Volumes[v].EmptyDir == nil || ps.Volumes[v].EmptyDir.SizeLimit == nil {
		t.Fatalf("volumes = %+v", ps.Volumes)
	}
}

// checkRunnerCopySecurity checks the runner-copy init container: non-root,
// RuntimeDefault seccomp, AppArmor left to the runtime, nothing to escalate.
func checkRunnerCopySecurity(t *testing.T, sc *corev1.SecurityContext) {
	t.Helper()
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || *sc.RunAsUser != 65532 || *sc.RunAsGroup != 65532 ||
		*sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || sc.AppArmorProfile != nil ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != dropAll {
		t.Fatalf("init security = %+v", sc)
	}
}

func TestTimeoutRounding(t *testing.T) {
	// Two 1500ms commands should round to 2s each, total 4s
	spec, total, err := sandbox.RunnerSpec([]lastv1alpha1.Command{
		{Run: lastv1alpha1.CommandRun{Argv: []string{"a"}}, Timeout: &metav1.Duration{Duration: 1500 * time.Millisecond}},
		{Run: lastv1alpha1.CommandRun{Argv: []string{"b"}}, Timeout: &metav1.Duration{Duration: 1500 * time.Millisecond}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Commands[0].TimeoutSeconds != 2 || spec.Commands[1].TimeoutSeconds != 2 {
		t.Fatalf("timeout seconds = %d, %d", spec.Commands[0].TimeoutSeconds, spec.Commands[1].TimeoutSeconds)
	}
	if total != 4*time.Second {
		t.Fatalf("total = %s, want 4s", total)
	}
}

func TestDefaultTimeoutFallback(t *testing.T) {
	// Zero and negative timeouts should use default
	for _, dur := range []time.Duration{0, -time.Second} {
		spec, _, err := sandbox.RunnerSpec([]lastv1alpha1.Command{
			{Run: lastv1alpha1.CommandRun{Argv: []string{"x"}}, Timeout: &metav1.Duration{Duration: dur}},
		})
		if err != nil {
			t.Fatalf("dur=%s: %v", dur, err)
		}
		if spec.Commands[0].TimeoutSeconds != 300 {
			t.Fatalf("dur=%s: got %d, want 300 (5m)", dur, spec.Commands[0].TimeoutSeconds)
		}
	}
}

func TestMaxTimeoutRejection(t *testing.T) {
	_, _, err := sandbox.RunnerSpec([]lastv1alpha1.Command{
		{ID: "toolong", Run: lastv1alpha1.CommandRun{Argv: []string{"x"}}, Timeout: &metav1.Duration{Duration: 7 * time.Hour}},
	})
	if err == nil || !strings.Contains(err.Error(), "toolong") || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("expected rejection with ID and 'exceeds maximum', got: %v", err)
	}
}

func TestSandboxPodDeadlineRounding(t *testing.T) {
	// 1500ms should round to 2s
	pod := sandbox.SandboxPod(sandbox.PodConfig{Namespace: lastSandboxNS, Image: testImage},
		testRunID, sandbox.Materialized{}, 1500*time.Millisecond)
	if *pod.Spec.ActiveDeadlineSeconds != 2 {
		t.Fatalf("deadline = %d, want 2", *pod.Spec.ActiveDeadlineSeconds)
	}

	// 0 should become 1
	pod = sandbox.SandboxPod(sandbox.PodConfig{Namespace: lastSandboxNS, Image: testImage},
		testRunID, sandbox.Materialized{}, 0)
	if *pod.Spec.ActiveDeadlineSeconds != 1 {
		t.Fatalf("deadline = %d, want 1", *pod.Spec.ActiveDeadlineSeconds)
	}
}

func TestDefaultResourceLimits(t *testing.T) {
	// Zero Resources should give defaults
	pod := sandbox.SandboxPod(sandbox.PodConfig{Namespace: lastSandboxNS, Image: testImage},
		testRunID, sandbox.Materialized{}, time.Minute)
	res := pod.Spec.Containers[0].Resources
	checkQuantity(t, res.Requests[corev1.ResourceCPU], "100m", "cpu request")
	checkQuantity(t, res.Requests[corev1.ResourceMemory], "256Mi", "memory request")
	checkQuantity(t, res.Requests[corev1.ResourceEphemeralStorage], "256Mi", "ephemeral storage request")
	checkQuantity(t, res.Limits[corev1.ResourceCPU], "1", "cpu limit")
	checkQuantity(t, res.Limits[corev1.ResourceMemory], "1Gi", "memory limit")
	checkQuantity(t, res.Limits[corev1.ResourceEphemeralStorage], "2Gi", "ephemeral storage limit")
}

func TestResourceOverlay(t *testing.T) {
	// Override memory limit only
	overrides := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	pod := sandbox.SandboxPod(sandbox.PodConfig{Namespace: lastSandboxNS, Image: testImage, Resources: overrides},
		testRunID, sandbox.Materialized{}, time.Minute)
	res := pod.Spec.Containers[0].Resources
	// Memory should be overridden
	checkQuantity(t, res.Limits[corev1.ResourceMemory], "512Mi", "memory limit")
	// CPU should keep default
	checkQuantity(t, res.Limits[corev1.ResourceCPU], "1", "cpu limit")
	// Ephemeral storage should keep default
	checkQuantity(t, res.Limits[corev1.ResourceEphemeralStorage], "2Gi", "ephemeral storage limit")
}

func TestSandboxPodComprehensiveHardening(t *testing.T) {
	volMount := corev1.VolumeMount{Name: "test-vol", MountPath: "/test"}
	vol := corev1.Volume{Name: "test-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	testEnv := corev1.EnvVar{Name: "TEST_VAR", Value: "test_value"}

	creds := sandbox.Materialized{
		Mounts:  []corev1.VolumeMount{volMount},
		Volumes: []corev1.Volume{vol},
		Env:     []corev1.EnvVar{testEnv},
	}

	pod := sandbox.SandboxPod(sandbox.PodConfig{Namespace: lastSandboxNS, Image: testImage},
		testRunID, creds, 7*time.Minute)

	assertPodSecurity(t, pod)
	assertPodPolicies(t, pod)
	assertSpecVolume(t, pod)
	assertCredentials(t, pod)
	assertServiceAccount(t, pod)
}

func assertPodSecurity(t *testing.T, pod *corev1.Pod) {
	ps := pod.Spec
	c := ps.Containers[0]

	// Pod-level security
	if !*ps.SecurityContext.RunAsNonRoot || *ps.SecurityContext.RunAsUser != 65532 ||
		*ps.SecurityContext.RunAsGroup != 65532 || *ps.SecurityContext.FSGroup != 65532 {
		t.Fatalf("pod security context = %+v", ps.SecurityContext)
	}

	// Container-level security
	if !*c.SecurityContext.RunAsNonRoot || *c.SecurityContext.AllowPrivilegeEscalation ||
		!*c.SecurityContext.ReadOnlyRootFilesystem || len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != dropAll {
		t.Fatalf("container security context = %+v", c.SecurityContext)
	}

	// Seccomp
	if ps.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("seccomp = %+v", ps.SecurityContext.SeccompProfile)
	}
}

func assertPodPolicies(t *testing.T, pod *corev1.Pod) {
	ps := pod.Spec

	// Pod policies
	if ps.RestartPolicy != corev1.RestartPolicyNever ||
		ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken ||
		ps.EnableServiceLinks == nil || *ps.EnableServiceLinks {
		t.Fatalf("pod policies = %+v", ps)
	}

	// Host namespace isolation
	if ps.HostNetwork || ps.HostPID || ps.HostIPC ||
		ps.ShareProcessNamespace == nil || *ps.ShareProcessNamespace {
		t.Fatalf("host isolation = host_network:%v host_pid:%v host_ipc:%v share_process:%v",
			ps.HostNetwork, ps.HostPID, ps.HostIPC,
			ps.ShareProcessNamespace != nil && *ps.ShareProcessNamespace)
	}
}

func assertSpecVolume(t *testing.T, pod *corev1.Pod) {
	ps := pod.Spec
	c := ps.Containers[0]

	// Spec volume with correct mode
	specVol := findVolume(ps.Volumes, "spec")
	if specVol == nil || specVol.Secret == nil || specVol.Secret.DefaultMode == nil ||
		*specVol.Secret.DefaultMode != 0o440 {
		t.Fatalf("spec volume = %+v", specVol)
	}

	// Spec volume mount as read-only
	specMount := findVolumeMount(c.VolumeMounts, "/last/spec")
	if specMount == nil || !specMount.ReadOnly {
		t.Fatalf("spec mount = %+v", specMount)
	}
}

func assertCredentials(t *testing.T, pod *corev1.Pod) {
	ps := pod.Spec
	c := ps.Containers[0]

	// Credential volumes and mounts present
	if findVolume(ps.Volumes, "test-vol") == nil {
		t.Fatalf("credential volume not found")
	}
	if findVolumeMount(c.VolumeMounts, "/test") == nil {
		t.Fatalf("credential mount not found")
	}

	// Environment: HOME first, then creds
	if len(c.Env) < 2 || c.Env[0].Name != "HOME" || c.Env[0].Value != workDir {
		t.Fatalf("env order = %+v", c.Env)
	}
	found := false
	for i := 1; i < len(c.Env); i++ {
		if c.Env[i].Name == "TEST_VAR" && c.Env[i].Value == "test_value" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("credential env not found in %+v", c.Env)
	}
}

func assertServiceAccount(t *testing.T, pod *corev1.Pod) {
	ps := pod.Spec
	if ps.ServiceAccountName != sandbox.SandboxServiceAccount {
		t.Fatalf("service account = %s, want %s", ps.ServiceAccountName, sandbox.SandboxServiceAccount)
	}
}

func findVolume(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		if vols[i].Name == name {
			return &vols[i]
		}
	}
	return nil
}

func findVolumeMount(mounts []corev1.VolumeMount, path string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].MountPath == path {
			return &mounts[i]
		}
	}
	return nil
}

func checkQuantity(t *testing.T, q resource.Quantity, expected, desc string) {
	if q.String() != expected {
		t.Fatalf("%s = %s, want %s", desc, q.String(), expected)
	}
}
