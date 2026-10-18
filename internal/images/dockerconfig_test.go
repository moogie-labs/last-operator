package images_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/moogie-labs/last-operator/internal/images"
)

const exampleHost = "reg.example"

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestCredentialsFor(t *testing.T) {
	cfg := []byte(`{"auths":{"reg.example":{"auth":"` + b64("u:p:w") + `"},"https://ghcr.io":{"auth":"` + b64("g:t") + `"},` +
		`"docker.example/v1/":{"auth":"` + b64("d:x") + `"},"empty.example":{}}}`)
	for host, want := range map[string]*images.Credentials{
		exampleHost:       {Username: "u", Password: "p:w"},
		"ghcr.io":         {Username: "g", Password: "t"},
		"docker.example":  {Username: "d", Password: "x"},
		"missing.example": nil,
		"empty.example":   nil,
	} {
		got, err := images.CredentialsFor(cfg, host)
		if err != nil || (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("CredentialsFor(%q) = %+v, %v; want %+v", host, got, err, want)
		}
	}
	for name, bad := range map[string]string{
		"json": "{", "base64": `{"auths":{"h":{"auth":"***"}}}`, "colon": `{"auths":{"h":{"auth":"` + b64("nocolon") + `"}}}`,
	} {
		if _, err := images.CredentialsFor([]byte(bad), "h"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDockerConfigRoundTrips(t *testing.T) {
	got, err := images.CredentialsFor(images.DockerConfig("h:5000", testPusher, "pw"), "h:5000")
	if err != nil || got == nil || got.Username != testPusher || got.Password != "pw" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestCredentialsForIsDeterministic(t *testing.T) {
	same := []byte(`{"auths":{"https://reg.example":{"auth":"` + b64("u:p") + `"},"reg.example/v1/":{"auth":"` + b64("u:p") + `"}}}`)
	for range 20 { // map order varies between runs; the answer must not
		got, err := images.CredentialsFor(same, exampleHost)
		if err != nil || got == nil || *got != (images.Credentials{Username: "u", Password: "p"}) {
			t.Fatalf("two keys with one credential: %+v, %v", got, err)
		}
	}
	exact := []byte(`{"auths":{"https://reg.example":{"auth":"` + b64("a:one") + `"},"reg.example":{"auth":"` + b64("b:two") +
		`"},"http://reg.example":{"auth":"` + b64("c:three") + `"}}}`)
	for range 20 {
		got, err := images.CredentialsFor(exact, exampleHost)
		if err != nil || got == nil || got.Username != "b" {
			t.Fatalf("an exact key must win: %+v, %v", got, err)
		}
	}
	conflict := []byte(`{"auths":{"https://reg.example":{"auth":"` + b64("a:conflict-one") + `"},"reg.example/v1/":{"auth":"` + b64("b:conflict-two") + `"}}}`)
	for range 20 {
		got, err := images.CredentialsFor(conflict, exampleHost)
		if err == nil || got != nil {
			t.Fatalf("two different credentials for one host: %+v, %v; want an error", got, err)
		}
		for _, leak := range []string{"conflict-one", "conflict-two", b64("a:conflict-one"), b64("b:conflict-two")} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("error %q leaks %q", err, leak)
			}
		}
	}
	// An empty entry holds no credential, so it conflicts with nothing.
	withEmpty := []byte(`{"auths":{"https://reg.example":{},"reg.example/v1/":{"auth":"` + b64("u:p") + `"}}}`)
	if got, err := images.CredentialsFor(withEmpty, exampleHost); err != nil || got == nil || got.Username != "u" {
		t.Fatalf("an empty entry beside a credential: %+v, %v", got, err)
	}
}
