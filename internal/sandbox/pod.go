package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	DefaultCommandTimeout = 5 * time.Minute
	MaxCommandTimeout     = 6 * time.Hour
	SandboxServiceAccount = "last-sandbox-runner"
	sandboxUID            = int64(65532)
	specVolume            = "spec"
	workVolume            = "work"
	tmpVolume             = "tmp"
	binVolume             = "last-bin"
	// runnerPath is where the sandbox base image holds the runner, and
	// runnerBinPath where a sandbox pod's init container copies it.
	runnerPath    = "/last/sandbox-runner"
	runnerBinPath = "/last-bin/sandbox-runner"
)

// DefaultSandboxResources returns the default CPU, memory, and storage limits for a sandbox pod.
func DefaultSandboxResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("100m"),
			corev1.ResourceMemory:           resource.MustParse("256Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("1"),
			corev1.ResourceMemory:           resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
	}
}

// RunnerSpec converts API commands into the runner's spec and returns the summed timeout.
func RunnerSpec(cmds []lastv1alpha1.Command) (runnerapi.Spec, time.Duration, error) {
	if len(cmds) == 0 {
		return runnerapi.Spec{}, 0, errors.New("sandbox: no commands to run")
	}
	spec := runnerapi.Spec{Version: runnerapi.Version}
	var total time.Duration
	for i, c := range cmds {
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
		if timeout > MaxCommandTimeout {
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("cmd-%d", i)
			}
			return runnerapi.Spec{}, 0, fmt.Errorf("sandbox: command %q: timeout %s exceeds maximum %s", id, timeout, MaxCommandTimeout)
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("cmd-%d", i)
		}
		timeoutSeconds := int64((timeout + time.Second - 1) / time.Second) // round up; never 0
		spec.Commands = append(spec.Commands, runnerapi.Command{
			ID: id, Argv: c.Run.Argv, Script: c.Run.Script, Files: c.Files,
			TimeoutSeconds: timeoutSeconds,
		})
		total += time.Duration(timeoutSeconds) * time.Second
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
	Namespace string
	// Image is the image the command runs in: the sandbox base, or an image
	// built from it whose install steps ran as root and may have replaced
	// anything in it, the runner included.
	Image string
	// RunnerImage is the pinned sandbox base the runner is copied from, so a
	// built image never decides what runs as the runner (spec §11.1). For a
	// command without install steps it equals Image.
	RunnerImage     string
	ImagePullPolicy corev1.PullPolicy
	// ImagePullSecrets name Secrets in Namespace that both containers pull with:
	// the copied external registry pull secret, or none for the bundled registry.
	ImagePullSecrets []string
	Resources        corev1.ResourceRequirements
}

// SandboxPod builds the locked-down pod for a run (spec §11.1).
func SandboxPod(cfg PodConfig, runID string, creds Materialized, activeDeadline time.Duration) *corev1.Pod {
	labels := RunLabels(runID)
	labels[LabelName] = NameValue
	no, yes := false, true
	uid := sandboxUID
	// Round up to whole seconds with floor of 1
	deadlineSecs := max(int64((activeDeadline+time.Second-1)/time.Second), 1)
	workLimit := resource.MustParse("1Gi")
	tmpLimit := resource.MustParse("64Mi")

	// Start from defaults and overlay cfg.Resources
	resources := DefaultSandboxResources()
	if cfg.Resources.Requests != nil {
		maps.Copy(resources.Requests, cfg.Resources.Requests)
	}
	if cfg.Resources.Limits != nil {
		maps.Copy(resources.Limits, cfg.Resources.Limits)
	}

	binLimit := resource.MustParse("64Mi")
	mounts := make([]corev1.VolumeMount, 0, 4+len(creds.Mounts))
	mounts = append(mounts,
		corev1.VolumeMount{Name: workVolume, MountPath: "/work"},
		corev1.VolumeMount{Name: tmpVolume, MountPath: "/tmp"},
		corev1.VolumeMount{Name: specVolume, MountPath: "/last/spec", ReadOnly: true},
		corev1.VolumeMount{Name: binVolume, MountPath: "/last-bin", ReadOnly: true},
	)
	mounts = append(mounts, creds.Mounts...)
	volumes := make([]corev1.Volume, 0, 4+len(creds.Volumes))
	volumes = append(volumes,
		corev1.Volume{Name: workVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &workLimit}}},
		corev1.Volume{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpLimit}}},
		corev1.Volume{Name: binVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &binLimit}}},
		corev1.Volume{Name: specVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: "last-run-" + runID + "-spec", DefaultMode: ptrInt32(0o440),
		}}},
	)
	volumes = append(volumes, creds.Volumes...)
	env := append([]corev1.EnvVar{{Name: "HOME", Value: "/work"}}, creds.Env...)
	var pullSecrets []corev1.LocalObjectReference // nil, not empty, without any
	if len(cfg.ImagePullSecrets) > 0 {
		pullSecrets = make([]corev1.LocalObjectReference, 0, len(cfg.ImagePullSecrets))
	}
	for _, name := range cfg.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "last-run-" + runID, Namespace: cfg.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			ServiceAccountName:           SandboxServiceAccount,
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			HostNetwork:                  false,
			HostPID:                      false,
			HostIPC:                      false,
			ShareProcessNamespace:        &no,
			ActiveDeadlineSeconds:        &deadlineSecs,
			ImagePullSecrets:             pullSecrets,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			// The runner comes from the pinned base, never from cfg.Image: a built
			// image's install steps ran as root and could have replaced
			// /last/sandbox-runner with one that skips the egress gate.
			InitContainers: []corev1.Container{{
				Name:            "runner-copy",
				Image:           cfg.RunnerImage,
				ImagePullPolicy: cfg.ImagePullPolicy,
				Command:         []string{"cp", runnerPath, runnerBinPath},
				VolumeMounts:    []corev1.VolumeMount{{Name: binVolume, MountPath: "/last-bin"}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid,
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					// AppArmorProfile stays unset, as on the build pod's init container: an
					// explicit RuntimeDefault makes kubelet reject the pod on a node without
					// AppArmor (kind on Docker Desktop: "Cannot enforce AppArmor").
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Containers: []corev1.Container{{
				Name:            RunnerContainer,
				Image:           cfg.Image,
				ImagePullPolicy: cfg.ImagePullPolicy,
				Command:         []string{runnerBinPath},
				Env:             env,
				VolumeMounts:    mounts,
				Resources:       resources,
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
