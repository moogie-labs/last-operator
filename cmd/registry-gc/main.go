// Command registry-gc is the sidecar of the bundled registry. It runs
// Distribution's garbage collection when the gateway asks for it.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/moogie-labs/last-operator/internal/registrygc"
)

// shutdownGrace is how long shutdown waits for an in-flight garbage collection.
const shutdownGrace = 25 * time.Minute

func main() {
	var cfg registrygc.Config
	addr := flag.String("addr", ":5001", "listen address")
	flag.StringVar(&cfg.HtpasswdFile, "htpasswd", "/etc/registry-gc/htpasswd", "htpasswd file holding the admin line")
	flag.StringVar(&cfg.ConfigFile, "config", "/etc/distribution/config.yml", "Distribution config file")
	flag.StringVar(&cfg.DataDir, "data", "/var/lib/registry", "registry storage directory")
	flag.Parse()

	// WriteTimeout stays unset: a garbage collection run may take minutes.
	srv := &http.Server{
		Addr:              *addr,
		Handler:           registrygc.Handler(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		slog.Info("Shutting down registry-gc")
		// Wait for an in-flight garbage collection; the pod's grace period matches.
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			slog.Error("Could not shut down registry-gc cleanly", "error", err)
		}
	}()
	slog.Info("Serving registry-gc", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("Registry-gc server stopped", "error", err)
		os.Exit(1)
	}
	// ListenAndServe returns as soon as Shutdown starts; wait for it to finish.
	<-done
}
