package sandbox_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	lastSandboxNS = "last-sandbox"
	testRunID     = "run1"
)

func TestRunnerSpecConvertsCommands(t *testing.T) {
	spec, total, err := sandbox.RunnerSpec([]lastv1alpha1.Command{
		{ID: "a", Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods"}}, Timeout: &metav1.Duration{Duration: time.Minute}},
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

func TestRunnerSpecRejectsImageBuildsAndBadCommands(t *testing.T) {
	_, _, err := sandbox.RunnerSpec([]lastv1alpha1.Command{{Run: lastv1alpha1.CommandRun{Argv: []string{"aws"}},
		Image: &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "awscli"}}}}}})
	if !errors.Is(err, sandbox.ErrImageBuildsUnsupported) {
		t.Fatalf("image err = %v", err)
	}
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
	pod := sandbox.SandboxPod(sandbox.PodConfig{Namespace: lastSandboxNS, Image: "img:tag", ImagePullPolicy: corev1.PullIfNotPresent},
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
	if c.Name != "runner" || c.Image != "img:tag" || *c.SecurityContext.AllowPrivilegeEscalation || !*c.SecurityContext.ReadOnlyRootFilesystem ||
		len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("container = %+v", c)
	}
	mounts := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = true
	}
	if !mounts["/work"] || !mounts["/tmp"] || !mounts["/last/spec"] {
		t.Fatalf("mounts = %+v", c.VolumeMounts)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["HOME"] != "/work" || env["DEMO"] != "x" {
		t.Fatalf("env = %+v", c.Env)
	}
	if pod.Labels[sandbox.LabelRun] != testRunID || pod.Labels[sandbox.LabelName] != sandbox.NameValue || pod.Name != "last-run-"+testRunID {
		t.Fatalf("meta = %+v", pod.ObjectMeta)
	}
}
