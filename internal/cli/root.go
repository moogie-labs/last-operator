// Package cli implements the kubectl-last plugin.
package cli

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	"github.com/moogie-labs/last-operator/internal/audit"
)

// pollInterval is how often a followed Intent is read again.
const pollInterval = 2 * time.Second

// Deps are the CLI's external dependencies, injectable for tests.
type Deps struct {
	Out, Err    io.Writer
	In          io.Reader
	Getenv      func(string) string
	NewSink     func(context.Context, audit.S3Config) (audit.Sink, error)
	NewS3Client func(context.Context, audit.S3Config) (*s3.Client, error)
	// Kube connects to the cluster as the user.
	Kube func(KubeFlags) (*Kube, error)
	// Wait pauses between reads of a followed Intent and returns ctx's error if it ends first.
	Wait func(context.Context) error
	// Suffix returns the random part of a generated Intent name.
	Suffix func() string
}

func DefaultDeps() Deps {
	return Deps{
		Out: os.Stdout, Err: os.Stderr, In: os.Stdin, Getenv: os.Getenv,
		NewS3Client: audit.NewS3Client,
		NewSink: func(ctx context.Context, cfg audit.S3Config) (audit.Sink, error) {
			c, err := audit.NewS3Client(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return audit.NewS3Sink(c, cfg.Bucket), nil
		},
		Kube:   NewKube,
		Wait:   pollWait,
		Suffix: func() string { return strings.ToLower(rand.Text()[:4]) },
	}
}

func pollWait(ctx context.Context) error {
	t := time.NewTimer(pollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func NewRootCommand(d Deps) *cobra.Command {
	kf := &KubeFlags{}
	root := &cobra.Command{
		Use:           "kubectl-last",
		Short:         "Work with last-operator",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(d.Out)
	root.SetErr(d.Err)
	kf.addTo(root)
	root.AddCommand(newAuditCommand(d), newPlanCommand(d, kf))
	return root
}
