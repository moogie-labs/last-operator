// Command audit-init provisions the bundled RustFS audit store (Helm Job).
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/moogie-labs/last-operator/internal/auditinit"
)

func main() {
	cfg, err := auditinit.ConfigFromEnv(os.Getenv)
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	if err := auditinit.Run(context.Background(), cfg); err != nil {
		slog.Error("audit store provisioning failed", "err", err)
		os.Exit(1)
	}
	slog.Info("audit store ready", "bucket", cfg.Bucket, "retentionDays", cfg.RetentionDays)
}
