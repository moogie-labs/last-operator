// Command sandbox-runner is the entrypoint of every sandbox pod. It runs the
// mounted spec and prints a single result envelope line for the gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/moogie-labs/last-operator/internal/sandbox/runner"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func main() {
	env, code := execute()
	out, err := json.Marshal(env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-runner: encode envelope:", err)
		os.Exit(2)
	}
	fmt.Printf("%s%s\n", runnerapi.ResultPrefix, out)
	os.Exit(code)
}

func execute() (runnerapi.Envelope, int) {
	failed := func(err error) (runnerapi.Envelope, int) {
		res := runnerapi.Result{ExitCode: -1, Error: err.Error()}
		return runnerapi.Envelope{Version: runnerapi.Version, Results: []runnerapi.Result{res}}, 2
	}
	raw, err := os.ReadFile(runnerapi.SpecPath)
	if err != nil {
		return failed(fmt.Errorf("read spec: %w", err))
	}
	spec, err := runnerapi.DecodeSpec(bytes.NewReader(raw))
	if err != nil {
		return failed(err)
	}
	return runner.Run(context.Background(), spec, runner.Options{
		MaxOutput: runnerapi.MaxOutput, WorkDir: "/work", Shell: "/bin/sh",
	}), 0
}
