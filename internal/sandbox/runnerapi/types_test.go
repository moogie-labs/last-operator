package runnerapi_test

import (
	"errors"
	"testing"

	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func TestParseEnvelopeFindsResultLine(t *testing.T) {
	log := []byte("some noise\n" + runnerapi.ResultPrefix + `{"version":1,"results":[{"id":"a","exitCode":0,"stdout":"hi","stderr":"","stdoutTruncated":false,"stderrTruncated":false}]}` + "\ntrailing noise\n")
	env, err := runnerapi.ParseEnvelope(log)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Results) != 1 || env.Results[0].ID != "a" || env.Results[0].Stdout != "hi" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestParseEnvelopeMissingResult(t *testing.T) {
	if _, err := runnerapi.ParseEnvelope([]byte("no result here\n")); !errors.Is(err, runnerapi.ErrNoResult) {
		t.Fatalf("err = %v, want ErrNoResult", err)
	}
}

func TestParseEnvelopeRejectsUnknownVersion(t *testing.T) {
	log := []byte(runnerapi.ResultPrefix + `{"version":2,"results":[]}` + "\n")
	if _, err := runnerapi.ParseEnvelope(log); err == nil {
		t.Fatal("accepted envelope version 2")
	}
}

func TestParseEnvelopeHandlesLargeLine(t *testing.T) {
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = 'a'
	}
	log := []byte(runnerapi.ResultPrefix + `{"version":1,"results":[{"id":"x","exitCode":0,"stdout":"` + string(big) + `","stderr":""}]}` + "\n")
	env, err := runnerapi.ParseEnvelope(log)
	if err != nil || len(env.Results[0].Stdout) != len(big) {
		t.Fatalf("large envelope: err=%v", err)
	}
}

func TestParseEnvelopeTwoResultLines(t *testing.T) {
	// Last result line should win
	log := []byte(
		runnerapi.ResultPrefix + `{"version":1,"results":[{"id":"first","exitCode":0,"stdout":"first"}]}` + "\n" +
			"noise\n" +
			runnerapi.ResultPrefix + `{"version":1,"results":[{"id":"second","exitCode":0,"stdout":"second"}]}` + "\n",
	)
	env, err := runnerapi.ParseEnvelope(log)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Results) != 1 || env.Results[0].ID != "second" || env.Results[0].Stdout != "second" {
		t.Fatalf("envelope = %+v", env)
	}
}
