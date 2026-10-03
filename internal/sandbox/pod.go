package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	DefaultCommandTimeout = 5 * time.Minute
	sandboxUID            = int64(65532)
	specVolume            = "spec"
	workVolume            = "work"
	tmpVolume             = "tmp"
)

// ErrImageBuildsUnsupported: image.install needs automatic image builds (spec §11.2, M4).
var ErrImageBuildsUnsupported = errors.New("sandbox: image.install is not supported until image builds (M4)")

// RunnerSpec converts API commands into the runner's spec and returns the summed timeout.
func RunnerSpec(cmds []lastv1alpha1.Command) (runnerapi.Spec, time.Duration, error) {
	if len(cmds) == 0 {
		return runnerapi.Spec{}, 0, errors.New("sandbox: no commands to run")
	}
	spec := runnerapi.Spec{Version: runnerapi.Version}
	var total time.Duration
	for i, c := range cmds {
		if c.Image != nil {
			return runnerapi.Spec{}, 0, ErrImageBuildsUnsupported
		}
		if (len(c.Run.Argv) == 0) == (c.Run.Script == "") {
			return runnerapi.Spec{}, 0, fmt.Errorf("sandbox: command %d: exactly one of argv or script must be set", i)
		}
		for p := range c.Files {
			if !underWork(p) {
				return runnerapi.Spec{}, 0, fmt.Errorf("sandbox: command %d: file path %q must be under /work/", i, p)
			}
		}
		timeout := DefaultCommandTimeout
		if c.Timeout != nil && c.Timeout.Duration > 0 {
			timeout = c.Timeout.Duration
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("cmd-%d", i)
		}
		spec.Commands = append(spec.Commands, runnerapi.Command{
			ID: id, Argv: c.Run.Argv, Script: c.Run.Script, Files: c.Files,
			TimeoutSeconds: int64((timeout + time.Second - 1) / time.Second), // round up; never 0
		})
		total += timeout
	}
	return spec, total, nil
}

// SpecSecret carries the run spec. It is a Secret because command files may hold
// sensitive content (e.g. manifests with embedded config).
func SpecSecret(namespace, runID string, spec runnerapi.Spec) (*corev1.Secret, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode spec: %w", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "last-run-" + runID + "-spec", Namespace: namespace, Labels: RunLabels(runID)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"spec.json": raw},
	}, nil
}

// PodConfig is the per-installation sandbox pod configuration.
type PodConfig struct {
	Namespace       string
	Image           string
	ImagePullPolicy corev1.PullPolicy
	Resources       corev1.ResourceRequirements
}

// SandboxPod builds the locked-down pod for a run (spec §11.1).
func SandboxPod(cfg PodConfig, runID string, creds Materialized, activeDeadline time.Duration) *corev1.Pod {
	labels := RunLabels(runID)
	labels[LabelName] = NameValue
	no, yes := false, true
	uid := sandboxUID
	deadline := int64(activeDeadline / time.Second)
	workLimit := resource.MustParse("1Gi")
	tmpLimit := resource.MustParse("64Mi")

	mounts := make([]corev1.VolumeMount, 0, 3+len(creds.Mounts))
	mounts = append(mounts,
		corev1.VolumeMount{Name: workVolume, MountPath: "/work"},
		corev1.VolumeMount{Name: tmpVolume, MountPath: "/tmp"},
		corev1.VolumeMount{Name: specVolume, MountPath: "/last/spec", ReadOnly: true},
	)
	mounts = append(mounts, creds.Mounts...)
	volumes := make([]corev1.Volume, 0, 3+len(creds.Volumes))
	volumes = append(volumes,
		corev1.Volume{Name: workVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &workLimit}}},
		corev1.Volume{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpLimit}}},
		corev1.Volume{Name: specVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: "last-run-" + runID + "-spec", DefaultMode: ptrInt32(0o440),
		}}},
	)
	volumes = append(volumes, creds.Volumes...)
	env := append([]corev1.EnvVar{{Name: "HOME", Value: "/work"}}, creds.Env...)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "last-run-" + runID, Namespace: cfg.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			ActiveDeadlineSeconds:        &deadline,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "runner",
				Image:           cfg.Image,
				ImagePullPolicy: cfg.ImagePullPolicy,
				Env:             env,
				VolumeMounts:    mounts,
				Resources:       cfg.Resources,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					RunAsNonRoot:             &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Volumes: volumes,
		},
	}
}
