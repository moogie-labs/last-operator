// Package cli implements the kubectl-last plugin.
package cli

import (
	"context"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	"github.com/moogie-labs/last-operator/internal/audit"
)

// Deps are the CLI's external dependencies, injectable for tests.
type Deps struct {
	Out, Err    io.Writer
	Getenv      func(string) string
	NewSink     func(context.Context, audit.S3Config) (audit.Sink, error)
	NewS3Client func(context.Context, audit.S3Config) (*s3.Client, error)
}

func DefaultDeps() Deps {
	return Deps{
		Out: os.Stdout, Err: os.Stderr, Getenv: os.Getenv,
		NewS3Client: audit.NewS3Client,
		NewSink: func(ctx context.Context, cfg audit.S3Config) (audit.Sink, error) {
			c, err := audit.NewS3Client(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return audit.NewS3Sink(c, cfg.Bucket), nil
		},
	}
}

func NewRootCommand(d Deps) *cobra.Command {
	root := &cobra.Command{
		Use:           "kubectl-last",
		Short:         "Work with last-operator",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(d.Out)
	root.SetErr(d.Err)
	root.AddCommand(newAuditCommand(d))
	return root
}
