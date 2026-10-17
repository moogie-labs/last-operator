package images

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func cmds(argvs ...[]string) []lastv1alpha1.ArgvCommand {
	out := make([]lastv1alpha1.ArgvCommand, len(argvs))
	for i, a := range argvs {
		out[i] = lastv1alpha1.ArgvCommand{Argv: a}
	}
	return out
}

func TestSpecHashIsTheHashOfTheCanonicalSpec(t *testing.T) {
	base := "sha256:" + strings.Repeat("a", 64)
	got, err := SpecHash(base, cmds([]string{"apt-get", "install", "-y", "awscli"}))
	if err != nil {
		t.Fatal(err)
	}
	lit := `{"baseDigest":"` + base + `","install":[{"argv":["apt-get","install","-y","awscli"]}]}`
	sum := sha256.Sum256([]byte(lit))
	if want := "sha256:" + hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if Tag(got) != "sha256-"+strings.TrimPrefix(got, "sha256:") {
		t.Fatalf("bad tag %s", Tag(got))
	}
}

func TestSpecHashChangesWithTheBaseAndEveryArgv(t *testing.T) {
	a := "sha256:" + strings.Repeat("a", 64)
	b := "sha256:" + strings.Repeat("b", 64)
	h := func(base string, c []lastv1alpha1.ArgvCommand) string {
		s, err := SpecHash(base, c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	orig := h(a, cmds([]string{"x", "y"}, []string{"z"}))
	for name, other := range map[string]string{
		"base":  h(b, cmds([]string{"x", "y"}, []string{"z"})),
		"argv":  h(a, cmds([]string{"x", "w"}, []string{"z"})),
		"order": h(a, cmds([]string{"z"}, []string{"x", "y"})),
	} {
		if other == orig {
			t.Errorf("%s change did not change the hash", name)
		}
	}
}

func TestBaseDigestNeedsADigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("0123456789abcdef", 4)
	got, err := BaseDigest("localhost:5000/last/base@" + d)
	if err != nil || got != d {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, ref := range []string{"ghcr.io/x/y:e2e", "x@sha256:short"} {
		if _, err := BaseDigest(ref); err == nil {
			t.Errorf("%s: want error", ref)
		}
	}
}

func TestContainerfileHasTheSpecForm(t *testing.T) {
	got, err := Containerfile("B", cmds([]string{"a", "b"}, []string{"c"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "FROM B\nUSER root\nRUN [\"a\",\"b\"]\nRUN [\"c\"]\nUSER 65532\n"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestContainerfileKeepsAwkwardArgvExact(t *testing.T) {
	argv := []string{"sh", "-c", "echo \"x\" \\ $HOME\nnext", "<&>", "é"}
	got, err := Containerfile("B", cmds(argv))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[2], "RUN ") {
		t.Fatalf("unexpected layout %q", got)
	}
	run := strings.TrimPrefix(lines[2], "RUN ")
	var back []string
	if err := json.Unmarshal([]byte(run), &back); err != nil {
		t.Fatal(err)
	}
	if strings.Join(back, "\x00") != strings.Join(argv, "\x00") {
		t.Fatalf("got %q", back)
	}
	if !strings.Contains(run, "<&>") {
		t.Fatalf("HTML escaped: %s", run)
	}
}

func TestContainerfileRefusesEmptyInstall(t *testing.T) {
	if _, err := Containerfile("B", nil); err == nil {
		t.Error("empty install: want error")
	}
	if _, err := Containerfile("B", cmds([]string{})); err == nil {
		t.Error("empty argv: want error")
	}
}
