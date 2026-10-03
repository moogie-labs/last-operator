// Package runnerapi is the contract between the gateway and the in-pod sandbox runner.
package runnerapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// SpecPath is where the gateway mounts the run spec inside the sandbox pod.
	SpecPath = "/last/spec/spec.json"
	// ResultPrefix marks the single log line that carries the result envelope.
	ResultPrefix = "LAST-RESULT "
	// Version is the spec/envelope format version.
	Version = 1
	// MaxOutput caps each of stdout and stderr per command (spec §11.1).
	MaxOutput = 1 << 20
)

// ErrNoResult means the pod log contained no result envelope.
var ErrNoResult = errors.New("runnerapi: sandbox produced no result envelope")

// Spec is what the runner executes, in order.
type Spec struct {
	Version  int       `json:"version"`
	Commands []Command `json:"commands"`
}

// Command is one execution. Exactly one of Argv or Script is set.
type Command struct {
	ID             string            `json:"id"`
	Argv           []string          `json:"argv,omitempty"`
	Script         string            `json:"script,omitempty"`
	Files          map[string]string `json:"files,omitempty"`
	TimeoutSeconds int64             `json:"timeoutSeconds"`
}

// Result is one command's outcome. ExitCode is -1 when the command could not run
// to completion (bad spec, start failure, timeout); Error then says why.
type Result struct {
	ID              string `json:"id"`
	ExitCode        int    `json:"exitCode"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated"`
	StderrTruncated bool   `json:"stderrTruncated"`
	Error           string `json:"error,omitempty"`
}

// Envelope is the runner's single output line.
type Envelope struct {
	Version int      `json:"version"`
	Results []Result `json:"results"`
}

// ParseEnvelope returns the last result envelope found in a pod log.
func ParseEnvelope(log []byte) (Envelope, error) {
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	var line []byte
	for sc.Scan() {
		if bytes.HasPrefix(sc.Bytes(), []byte(ResultPrefix)) {
			line = bytes.Clone(sc.Bytes()[len(ResultPrefix):])
		}
	}
	if err := sc.Err(); err != nil {
		return Envelope{}, fmt.Errorf("runnerapi: read log: %w", err)
	}
	if line == nil {
		return Envelope{}, ErrNoResult
	}
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Envelope{}, fmt.Errorf("runnerapi: parse envelope: %w", err)
	}
	if env.Version != Version {
		return Envelope{}, fmt.Errorf("runnerapi: unsupported envelope version %d", env.Version)
	}
	return env, nil
}

// DecodeSpec reads a spec the way the in-pod runner must: unknown fields and a
// version mismatch are errors, so a gateway/runner skew fails loudly instead of
// silently dropping fields.
func DecodeSpec(r io.Reader) (Spec, error) {
	var spec Spec
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("parse spec: %w", err)
	}
	if spec.Version != Version {
		return Spec{}, fmt.Errorf("unsupported spec version %d", spec.Version)
	}
	return spec, nil
}
