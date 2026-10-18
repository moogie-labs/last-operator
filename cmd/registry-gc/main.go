// Command registry-gc is the sidecar of the bundled registry. It runs
// Distribution's garbage collection when the gateway asks for it.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/moogie-labs/last-operator/internal/registrygc"
)

func main() {
	var cfg registrygc.Config
	addr := flag.String("addr", ":5001", "listen address")
	flag.StringVar(&cfg.HtpasswdFile, "htpasswd", "/etc/registry-gc/htpasswd", "htpasswd file holding the admin line")
	flag.StringVar(&cfg.ConfigFile, "config", "/etc/distribution/config.yml", "Distribution config file")
	flag.StringVar(&cfg.DataDir, "data", "/var/lib/registry", "registry storage directory")
	flag.Parse()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           registrygc.Handler(cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("Serving registry-gc", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("Registry-gc server stopped", "error", err)
		os.Exit(1)
	}
}
