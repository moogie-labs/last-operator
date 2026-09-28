// Command gateway is the trusted last-operator component that holds credentials
// and writes the write-once audit log.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := gateway.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	key, err := gateway.EnsureSigningKey(ctx, kube, cfg.Namespace, cfg.SigningKeySecret, cfg.PublicKeyConfigMap)
	if err != nil {
		return err
	}
	client, err := audit.NewS3Client(ctx, cfg.Audit)
	if err != nil {
		return err
	}
	g := gateway.New(cfg, kube, audit.NewS3Sink(client, cfg.Audit.Bucket), audit.NewSigner(key))
	return g.Run(ctx)
}
