// Command kubectl-last is the last-operator kubectl plugin.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/moogie-labs/last-operator/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := cli.NewRootCommand(cli.DefaultDeps()).ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
