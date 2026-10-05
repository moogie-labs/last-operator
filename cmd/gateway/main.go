// Command gateway is the trusted last-operator component that holds credentials
// and writes the write-once audit log.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
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
	client3, err := audit.NewS3Client(ctx, cfg.Audit)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := lastv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	crd, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	g := gateway.New(cfg, kube, crd, audit.NewS3Sink(client3, cfg.Audit.Bucket), audit.NewSigner(key))
	return g.Run(ctx)
}
