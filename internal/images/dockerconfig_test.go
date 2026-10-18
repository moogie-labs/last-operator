package images_test

import (
	"encoding/base64"
	"testing"

	"github.com/moogie-labs/last-operator/internal/images"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestCredentialsFor(t *testing.T) {
	cfg := []byte(`{"auths":{"reg.example":{"auth":"` + b64("u:p:w") + `"},"https://ghcr.io":{"auth":"` + b64("g:t") + `"},` +
		`"docker.example/v1/":{"auth":"` + b64("d:x") + `"},"empty.example":{}}}`)
	for host, want := range map[string]*images.Credentials{
		"reg.example":     {Username: "u", Password: "p:w"},
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
