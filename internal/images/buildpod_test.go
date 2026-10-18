package images_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/images"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	testFront  = "last-registry-front.last-registry.svc:5000"
	testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testNS     = "last-build"
	testPusher = "pusher"
	testPW     = "s3cret"
	testHash   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func testCfg() images.BuildPodConfig {
	return images.BuildPodConfig{
		Namespace: testNS, BuildKitImage: "moby/buildkit:v0.26.2-rootless",
		RunnerImage: "sandbox@" + testDigest, FrontHost: testFront,
		PushRef: testFront + "/" + images.ImagesRepo, Insecure: true,
		Timeout: 15 * time.Minute, Canaries: []string{"10.0.0.1:443"},
	}
}

func testBuild() images.BuildSpec {
	return images.BuildSpec{
		ID: "b1", SpecHash: testHash, Base: "localhost:5000/last/base@" + testDigest,
		Install:    []lastv1alpha1.ArgvCommand{{Argv: []string{"apt-get", "install", "-y", "jq"}}},
		PushConfig: images.DockerConfig(testFront, testPusher, testPW),
		Egress:     []sandbox.EgressRule{{CIDR: "10.96.5.5/32", Port: 5000}},
	}
}

func buildSpec(t *testing.T, cfg images.BuildPodConfig, b images.BuildSpec) runnerapi.Spec {
	t.Helper()
	sec, _, _, err := images.BuildObjects(cfg, b, time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := runnerapi.DecodeSpec(bytes.NewReader(sec.Data["spec.json"]))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestBuildObjectsRunTheRunnerUnderBuildKit(t *testing.T) {
	cfg := testCfg()
	sec, np, pod, err := images.BuildObjects(cfg, testBuild(), time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]string{sec.Labels, np.Labels, pod.Labels} {
		if m[sandbox.LabelManagedBy] != sandbox.ManagedByValue || m[images.LabelBuild] != "b1" ||
			m[sandbox.LabelName] != "last-build" || m[sandbox.LabelExpiresAt] != "2000" {
			t.Fatalf("labels = %v", m)
		}
	}
	if pod.Name != "last-build-b1" || np.Name != "last-build-b1" || sec.Name != "last-build-b1-spec" || pod.Namespace != testNS {
		t.Fatalf("names: %s %s %s", pod.Name, np.Name, sec.Name)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("init containers = %d", len(pod.Spec.InitContainers))
	}
	init := pod.Spec.InitContainers[0]
	if init.Image != cfg.RunnerImage || !slices.Equal(init.Command, []string{"cp", "/last/sandbox-runner", "/last-bin/sandbox-runner"}) ||
		*init.SecurityContext.RunAsUser != 1000 || init.VolumeMounts[0].Name != "last-bin" {
		t.Fatalf("init = %+v", init)
	}
	c := pod.Spec.Containers[0]
	if c.Image != cfg.BuildKitImage || !slices.Equal(c.Command, []string{"/last-bin/sandbox-runner"}) {
		t.Fatalf("container = %s %v", c.Image, c.Command)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["BUILDKITD_FLAGS"] != "--oci-worker-no-process-sandbox --config /work/buildkitd.toml" ||
		env["DOCKER_CONFIG"] != "/work/.docker" || env["HOME"] != "/home/user" {
		t.Fatalf("env = %v", env)
	}
	for _, e := range c.Env {
		if strings.Contains(e.Value, "s3cret") {
			t.Fatal("password in env")
		}
	}
	checkBuildSecurity(t, pod, c)
	checkBuildVolumes(t, sec.Name, pod, c)
}

func checkBuildSecurity(t *testing.T, pod *corev1.Pod, c corev1.Container) {
	t.Helper()
	sc := c.SecurityContext
	if *sc.RunAsUser != 1000 || *sc.RunAsGroup != 1000 ||
		sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined || sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined {
		t.Fatalf("securityContext = %+v", sc)
	}
	psc := pod.Spec.SecurityContext
	// Nothing relaxed at pod level, or the init container would inherit it.
	if *psc.RunAsUser != 1000 || *psc.RunAsGroup != 1000 || *psc.FSGroup != 1000 ||
		psc.SeccompProfile != nil || psc.AppArmorProfile != nil {
		t.Fatalf("pod securityContext = %+v", psc)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("service account token must not be mounted")
	}
	lim := c.Resources.Limits
	if lim.Cpu().String() != "2" || lim.Memory().String() != "4Gi" || lim.StorageEphemeral().String() != "10Gi" {
		t.Fatalf("limits = %v", lim)
	}
	if *pod.Spec.ActiveDeadlineSeconds != int64((15*time.Minute+3*time.Minute)/time.Second) {
		t.Fatalf("deadline = %d", *pod.Spec.ActiveDeadlineSeconds)
	}
	if c.SecurityContext.AllowPrivilegeEscalation != nil && !*c.SecurityContext.AllowPrivilegeEscalation {
		// no_new_privs breaks rootlesskit: "newuidmap: Could not set caps".
		t.Fatal("BuildKit container must not set allowPrivilegeEscalation=false")
	}
	init := pod.Spec.InitContainers[0]
	is := init.SecurityContext
	if is.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || is.AppArmorProfile != nil ||
		is.AllowPrivilegeEscalation == nil || *is.AllowPrivilegeEscalation || *is.RunAsGroup != 1000 ||
		len(is.Capabilities.Drop) != 1 || is.Capabilities.Drop[0] != "ALL" ||
		init.Resources.Requests.Cpu().String() != "10m" || init.Resources.Requests.Memory().String() != "16Mi" || init.Resources.Limits.Memory().String() != "32Mi" {
		t.Fatalf("init securityContext/resources = %+v %+v", is, init.Resources)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restart = %s", pod.Spec.RestartPolicy)
	}
}

func checkBuildVolumes(t *testing.T, secretName string, pod *corev1.Pod, c corev1.Container) {
	t.Helper()
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m.Name
	}
	for _, p := range []string{"/last/spec", "/work", "/last-bin", "/home/user/.local/share/buildkit"} {
		if mounts[p] == "" {
			t.Fatalf("missing mount %s: %v", p, mounts)
		}
	}
	vols := map[string]corev1.VolumeSource{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v.VolumeSource
	}
	if vols[mounts["/last/spec"]].Secret == nil || vols[mounts["/last/spec"]].Secret.SecretName != secretName {
		t.Fatalf("spec volume = %+v", vols[mounts["/last/spec"]])
	}
	for _, p := range []string{"/work", "/last-bin", "/home/user/.local/share/buildkit"} {
		if vols[mounts[p]].EmptyDir == nil {
			t.Fatalf("%s is not an emptyDir", p)
		}
	}
	if vols["last-bin"].EmptyDir == nil {
		t.Fatal("last-bin volume missing")
	}
}

func TestBuildObjectsCarryTheBuildInTheRunnerSpec(t *testing.T) {
	cfg, b := testCfg(), testBuild()
	spec := buildSpec(t, cfg, b)
	if len(spec.Commands) != 1 || spec.Commands[0].Script == "" || len(spec.Commands[0].Argv) != 0 {
		t.Fatalf("commands = %+v", spec.Commands)
	}
	cmd := spec.Commands[0]
	want := "buildctl-daemonless.sh build --frontend dockerfile.v0 --local context=/work/ctx --local dockerfile=/work/ctx " +
		"--output type=image,name=" + testFront + "/last/images:" + images.Tag(testHash) + ",push=true,registry.insecure=true " +
		"--metadata-file /work/meta.json --opt filename=Containerfile >/work/build.log 2>&1\n"
	// The flags in order, then the log tail on stderr, buildctl's exit code kept, and
	// only meta.json on stdout.
	wantScript := want + "rc=$?\ntail -c 1000000 /work/build.log >&2\n[ \"$rc\" -eq 0 ] || exit \"$rc\"\ncat /work/meta.json\n"
	if cmd.Script != wantScript || strings.Contains(cmd.Script, "set -e") {
		t.Fatalf("script = %q", cmd.Script)
	}
	if cmd.TimeoutSeconds != int64(cfg.Timeout/time.Second) {
		t.Fatalf("timeout = %d", cmd.TimeoutSeconds)
	}
	rewritten := testFront + "/last/base@" + testDigest
	wantCF, err := images.Containerfile(rewritten, b.Install)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Files["/work/ctx/Containerfile"] != wantCF {
		t.Fatalf("Containerfile = %q", cmd.Files["/work/ctx/Containerfile"])
	}
	toml := cmd.Files["/work/buildkitd.toml"]
	if !strings.Contains(toml, `[registry."`+testFront+`"]`) || !strings.Contains(toml, "http = true") {
		t.Fatalf("buildkitd.toml = %q", toml)
	}
	var dc struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal([]byte(cmd.Files["/work/.docker/config.json"]), &dc); err != nil {
		t.Fatal(err)
	}
	if dc.Auths[testFront].Auth != base64.StdEncoding.EncodeToString([]byte(testPusher+":s3cret")) {
		t.Fatalf("auths = %+v", dc.Auths)
	}
	if !slices.Equal(spec.EgressCanaries, cfg.Canaries) {
		t.Fatalf("canaries = %v", spec.EgressCanaries)
	}
}

func TestBuildObjectsRewriteABundledBase(t *testing.T) {
	spec := buildSpec(t, testCfg(), testBuild())
	if !strings.HasPrefix(spec.Commands[0].Files["/work/ctx/Containerfile"], "FROM "+testFront+"/last/base@"+testDigest+"\n") {
		t.Fatalf("Containerfile = %q", spec.Commands[0].Files["/work/ctx/Containerfile"])
	}
	b := testBuild()
	b.Base = "ghcr.io/acme/base@" + testDigest
	spec = buildSpec(t, testCfg(), b)
	if !strings.HasPrefix(spec.Commands[0].Files["/work/ctx/Containerfile"], "FROM ghcr.io/acme/base@"+testDigest+"\n") {
		t.Fatalf("Containerfile = %q", spec.Commands[0].Files["/work/ctx/Containerfile"])
	}
}

func TestBuildObjectsRewriteWithACustomPullHost(t *testing.T) {
	cfg := testCfg()
	cfg.PullHost = "pull.example:5001"
	b := testBuild()
	b.Base = "pull.example:5001/last/base@" + testDigest
	spec := buildSpec(t, cfg, b)
	if !strings.HasPrefix(spec.Commands[0].Files["/work/ctx/Containerfile"], "FROM "+testFront+"/last/base@"+testDigest+"\n") {
		t.Fatalf("Containerfile = %q", spec.Commands[0].Files["/work/ctx/Containerfile"])
	}
	// The default host is no longer special.
	b.Base = "localhost:5000/last/base@" + testDigest
	spec = buildSpec(t, cfg, b)
	if !strings.HasPrefix(spec.Commands[0].Files["/work/ctx/Containerfile"], "FROM localhost:5000/") {
		t.Fatalf("Containerfile = %q", spec.Commands[0].Files["/work/ctx/Containerfile"])
	}
}

func TestBuildObjectsKeepTheCredentialInTheSpecSecretOnly(t *testing.T) {
	sec, np, pod, err := images.BuildObjects(testCfg(), testBuild(), time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{"s3cret", base64.StdEncoding.EncodeToString([]byte(testPusher + ":s3cret"))}
	spec := buildSpec(t, testCfg(), testBuild())
	var haystack []string
	haystack = append(haystack, spec.Commands[0].Script)
	for _, c := range append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...) {
		haystack = append(haystack, c.Command...)
		haystack = append(haystack, c.Args...)
		for _, e := range c.Env {
			haystack = append(haystack, e.Name, e.Value)
		}
	}
	for _, m := range []map[string]string{sec.Labels, sec.Annotations, np.Labels, np.Annotations, pod.Labels, pod.Annotations} {
		for k, v := range m {
			haystack = append(haystack, k, v)
		}
	}
	npJSON, err := json.Marshal(np)
	if err != nil {
		t.Fatal(err)
	}
	podJSON, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	haystack = append(haystack, string(npJSON), string(podJSON))
	for _, h := range haystack {
		for _, s := range secrets {
			if strings.Contains(h, s) {
				t.Fatalf("credential %q found in %q", s, h)
			}
		}
	}
	if !strings.Contains(spec.Commands[0].Files["/work/.docker/config.json"], secrets[1]) {
		t.Fatal("credential missing from the spec Secret's config.json")
	}
}

func TestBuildPolicyAllowsOnlyTheGrantsAndDNS(t *testing.T) {
	b := testBuild()
	b.Egress = append(b.Egress, sandbox.EgressRule{CIDR: "203.0.113.0/24", Port: 443})
	_, np, _, err := images.BuildObjects(testCfg(), b, time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if np.Spec.PodSelector.MatchLabels[images.LabelBuild] != "b1" || len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != "Egress" || len(np.Spec.Ingress) != 0 {
		t.Fatalf("policy = %+v", np.Spec)
	}
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("egress = %+v", np.Spec.Egress)
	}
	for i, r := range b.Egress {
		e := np.Spec.Egress[i]
		if e.To[0].IPBlock.CIDR != r.CIDR || len(e.Ports) != 1 || *e.Ports[0].Protocol != corev1.ProtocolTCP || e.Ports[0].Port.IntVal != r.Port {
			t.Fatalf("rule %d = %+v", i, e)
		}
	}
	dns := np.Spec.Egress[2]
	if len(dns.To) != 1 || dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" ||
		dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" || len(dns.Ports) != 2 {
		t.Fatalf("dns rule = %+v", dns)
	}
	protos := map[corev1.Protocol]bool{}
	for _, p := range dns.Ports {
		if p.Port.IntVal != 53 {
			t.Fatalf("dns port = %v", p.Port)
		}
		protos[*p.Protocol] = true
	}
	if !protos[corev1.ProtocolUDP] || !protos[corev1.ProtocolTCP] {
		t.Fatalf("dns protocols = %v", protos)
	}
}

const extRef = "123.dkr.ecr.us-east-1.amazonaws.com/last-images"

func TestBuildObjectsPushToTheExternalRepository(t *testing.T) {
	cfg, b := testCfg(), testBuild()
	cfg.PushRef, cfg.Insecure = extRef, false
	b.PushConfig = images.DockerConfig("123.dkr.ecr.us-east-1.amazonaws.com", "AWS", "tok")
	b.Base = "ghcr.io/acme/base@" + testDigest
	cmd := buildSpec(t, cfg, b).Commands[0]
	if !strings.Contains(cmd.Script, "name="+extRef+":"+images.Tag(testHash)+",push=true ") || strings.Contains(cmd.Script, "registry.insecure") {
		t.Fatalf("script = %q", cmd.Script)
	}
	if toml := cmd.Files["/work/buildkitd.toml"]; strings.Contains(toml, "[registry") || strings.Contains(toml, "http =") {
		t.Fatalf("buildkitd.toml = %q", toml)
	}
	if _, ok := cmd.Files["/work/buildkitd.toml"]; !ok {
		t.Fatal("buildkitd.toml missing")
	}
	// An external build leaves the base alone, even under the pull host.
	b.Base = "localhost:5000/last/base@" + testDigest
	cmd = buildSpec(t, cfg, b).Commands[0]
	if !strings.HasPrefix(cmd.Files["/work/ctx/Containerfile"], "FROM localhost:5000/last/base@") {
		t.Fatalf("Containerfile = %q", cmd.Files["/work/ctx/Containerfile"])
	}
}

func TestBuildObjectsRejectAMalformedPushConfig(t *testing.T) {
	for name, cfgJSON := range map[string]string{
		"not json": "nope", "bad base64": `{"auths":{"h":{"auth":"***"}}}`, "no colon": `{"auths":{"h":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("nocolon")) + `"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := testBuild()
			b.PushConfig = []byte(cfgJSON)
			_, _, _, err := images.BuildObjects(testCfg(), b, time.Unix(2000, 0))
			if err == nil || strings.Contains(err.Error(), "nocolon") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestParseRepository(t *testing.T) {
	for in, want := range map[string][2]string{
		"reg.example/last-images":           {"reg.example", "last-images"},
		"reg.example:5443/team/last_images": {"reg.example:5443", "team/last_images"},
		extRef:                              {"123.dkr.ecr.us-east-1.amazonaws.com", "last-images"},
		"ghcr.io/acme/tools.images/a__b":    {"ghcr.io", "acme/tools.images/a__b"},
		"10.0.0.5:5000/last/images":         {"10.0.0.5:5000", "last/images"},
		"localhost:5000/ab":                 {"localhost:5000", "ab"},
	} {
		host, path, err := images.ParseRepository(in)
		if err != nil || host != want[0] || path != want[1] {
			t.Errorf("ParseRepository(%q) = %q, %q, %v; want %q, %q", in, host, path, err, want[0], want[1])
		}
	}
	for _, in := range []string{
		"", exampleHost, "reg.example/", "/last-images", "reg.example//x", "reg.example/x/",
		"https://reg.example/x", "user:hunter2@reg.example/x", "reg.example/x y", "reg.example/x\ty", " reg.example/x",
		"reg.example/x,push=false", `reg.example/x"`, "reg.example/x'", "reg.example/x;id", "reg.example/$(id)",
		"reg.example/x`id`", "reg.example/x|y", "reg.example/x&y", "reg.example/x>y", "reg.example/x\ny",
		"reg.example/X", "reg.example/x:tag", "reg.example/x@sha256:abc", "reg.example:port/x", "reg_example/x",
		"reg.example/" + strings.Repeat("a", 256),
		// A dotless first segment is a Docker Hub namespace to every client.
		"harbor/acme/x", "localhost/x", "harbor/x",
		// go-containerregistry needs at least two characters of path.
		"reg.example/a",
	} {
		_, _, err := images.ParseRepository(in)
		if err == nil {
			t.Errorf("ParseRepository(%q) succeeded, want an error", in)
			continue
		}
		if in != "" && strings.Contains(err.Error(), in) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("ParseRepository(%q) error %q echoes the value", in, err)
		}
	}
}

func TestBuildObjectsRejectAnInvalidExternalRepository(t *testing.T) {
	cfg := testCfg()
	cfg.PushRef, cfg.Insecure = "reg.example/x,push=false", false
	if _, _, _, err := images.BuildObjects(cfg, testBuild(), time.Unix(2000, 0)); err == nil {
		t.Fatal("BuildObjects accepted a repository that would rewrite BuildKit's --output")
	}
}
