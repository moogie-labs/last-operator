package images

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	// LabelBuild carries the build ID on every object of a build.
	LabelBuild = "last.moogie-labs.com/build"

	buildNameValue = "last-build"
	// bundledPullHost is the default PullHost (spec §11.3).
	bundledPullHost = "localhost:5000"
	buildUID        = int64(1000)
	// buildDeadlineSlack is added to the build timeout: activeDeadlineSeconds counts
	// from pod start, so it covers the BuildKit image pull, the init container and the
	// runner's egress gate (up to 30 s) before the runner's own timeout begins.
	buildDeadlineSlack = 3 * time.Minute

	binVolume   = "last-bin"
	workVolume  = "work"
	specVolume  = "spec"
	cacheVolume = "buildkit"
)

// BuildPodConfig is the per-installation build pod configuration.
type BuildPodConfig struct {
	Namespace, BuildKitImage string
	// RunnerImage is the pinned sandbox base, which holds /last/sandbox-runner.
	RunnerImage string
	// PullHost is how nodes reach the bundled registry; empty means "localhost:5000".
	// A bundled base reference under it is rewritten to FrontHost for the build.
	PullHost string
	// FrontHost is the registry front proxy's host:port, as seen from the build pod.
	FrontHost string
	Timeout   time.Duration
	// Canaries are passed to the runner's egress gate.
	Canaries []string
}

// BuildSpec is one build.
type BuildSpec struct {
	ID, SpecHash string
	// Base is the pinned base reference, as the sandbox runs it.
	Base    string
	Install []lastv1alpha1.ArgvCommand
	// PushUser and PushPassword appear only in the spec Secret's config.json.
	PushUser, PushPassword string
	// Egress holds the registry rules and the allowed buildEgress entries. BuildObjects
	// itself adds the kube-dns rule (UDP and TCP 53), so callers must not.
	Egress []sandbox.EgressRule
}

func buildLabels(id string, expires time.Time) map[string]string {
	return map[string]string{
		sandbox.LabelManagedBy: sandbox.ManagedByValue,
		LabelBuild:             id,
		sandbox.LabelName:      buildNameValue,
		sandbox.LabelExpiresAt: strconv.FormatInt(expires.Unix(), 10),
	}
}

// BuildObjects returns the spec Secret, the NetworkPolicy and the pod for one build.
// The pod runs rootless BuildKit under the sandbox runner, which gives it the egress
// gate and the result envelope (spike finding 6, design §4.2).
func BuildObjects(cfg BuildPodConfig, b BuildSpec, expires time.Time) (*corev1.Secret, *networkingv1.NetworkPolicy, *corev1.Pod, error) {
	if b.ID == "" {
		return nil, nil, nil, errors.New("build has no ID")
	}
	base := b.Base
	pullHost := cfg.PullHost
	if pullHost == "" {
		pullHost = bundledPullHost
	}
	if rest, ok := strings.CutPrefix(base, pullHost+"/"); ok {
		base = cfg.FrontHost + "/" + rest
	}
	containerfile, err := Containerfile(base, b.Install)
	if err != nil {
		return nil, nil, nil, err
	}
	dockerCfg, err := json.Marshal(map[string]any{"auths": map[string]any{
		cfg.FrontHost: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(b.PushUser + ":" + b.PushPassword))},
	}})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encoding registry auth: %w", err)
	}
	timeoutSeconds := max(int64((cfg.Timeout+time.Second-1)/time.Second), 1)
	// buildctl's output goes to a file and the runner keeps only the first MaxOutput
	// bytes of stderr, so emit the last ~1 MB of the log. Only meta.json reaches stdout.
	script := "buildctl-daemonless.sh build --frontend dockerfile.v0" +
		" --local context=/work/ctx --local dockerfile=/work/ctx" +
		" --output type=image,name=" + cfg.FrontHost + "/last/images:" + Tag(b.SpecHash) + ",push=true,registry.insecure=true" +
		" --metadata-file /work/meta.json --opt filename=Containerfile >/work/build.log 2>&1\n" +
		"rc=$?\n" +
		"tail -c 1000000 /work/build.log >&2\n" +
		"[ \"$rc\" -eq 0 ] || exit \"$rc\"\n" +
		"cat /work/meta.json\n"
	spec := runnerapi.Spec{
		Version: runnerapi.Version,
		Commands: []runnerapi.Command{{
			ID: "build", Script: script, TimeoutSeconds: timeoutSeconds,
			Files: map[string]string{
				"/work/ctx/Containerfile":   containerfile,
				"/work/buildkitd.toml":      "[registry.\"" + cfg.FrontHost + "\"]\n  http = true\n",
				"/work/.docker/config.json": string(dockerCfg),
			},
		}},
		EgressCanaries: cfg.Canaries,
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encoding build spec: %w", err)
	}

	name := "last-build-" + b.ID
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-spec", Namespace: cfg.Namespace, Labels: buildLabels(b.ID, expires)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"spec.json": raw},
	}
	return secret, buildPolicy(cfg.Namespace, name, b, expires), buildPod(cfg, name, b.ID, secret.Name, timeoutSeconds, expires), nil
}

func buildPolicy(namespace, name string, b BuildSpec, expires time.Time) *networkingv1.NetworkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: buildLabels(b.ID, expires)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelBuild: b.ID}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
	for _, r := range b.Egress {
		port := intstr.FromInt32(r.Port)
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: r.CIDR}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		})
	}
	dns := intstr.FromInt32(53)
	np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
	})
	return np
}

func buildPod(cfg BuildPodConfig, name, id, secretName string, timeoutSeconds int64, expires time.Time) *corev1.Pod {
	no := false
	uid := buildUID
	deadline := timeoutSeconds + int64(buildDeadlineSlack/time.Second)
	// The BuildKit container must not set allowPrivilegeEscalation=false: no_new_privs
	// stops rootlesskit's newuidmap/newgidmap (privileged by file capabilities) with
	// "newuidmap: Could not set caps", so buildkitd never starts.
	buildSecCtx := func() *corev1.SecurityContext {
		return &corev1.SecurityContext{
			RunAsUser: &uid, RunAsGroup: &uid,
			SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		}
	}
	mode := int32(0o440)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.Namespace, Labels: buildLabels(id, expires)},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			ActiveDeadlineSeconds:        &deadline,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
				SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
				AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
			},
			InitContainers: []corev1.Container{{
				Name:         "runner-copy",
				Image:        cfg.RunnerImage,
				Command:      []string{"cp", "/last/sandbox-runner", "/last-bin/sandbox-runner"},
				VolumeMounts: []corev1.VolumeMount{{Name: binVolume, MountPath: "/last-bin"}},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: &uid, RunAsGroup: &uid, AllowPrivilegeEscalation: &no,
					SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault},
					Capabilities:    &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
				},
			}},
			Containers: []corev1.Container{{
				Name:    "build",
				Image:   cfg.BuildKitImage,
				Command: []string{"/last-bin/sandbox-runner"},
				Env: []corev1.EnvVar{
					{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox --config /work/buildkitd.toml"},
					{Name: "DOCKER_CONFIG", Value: "/work/.docker"},
					{Name: "HOME", Value: "/home/user"},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: specVolume, MountPath: "/last/spec", ReadOnly: true},
					{Name: workVolume, MountPath: "/work"},
					{Name: binVolume, MountPath: "/last-bin"},
					{Name: cacheVolume, MountPath: "/home/user/.local/share/buildkit"},
				},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("2"),
						corev1.ResourceMemory:           resource.MustParse("4Gi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
					},
				},
				SecurityContext: buildSecCtx(),
			}},
			Volumes: []corev1.Volume{
				{Name: specVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName, DefaultMode: &mode}}},
				{Name: workVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: binVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: cacheVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
}
