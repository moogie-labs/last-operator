package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/moogie-labs/last-operator/internal/audit"
)

var ErrVerifyFailed = errors.New("audit chain failed verification")

type auditFlags struct {
	endpoint, bucket, region, chain string
	pathStyle                       bool
}

func envOr(d Deps, key, def string) string {
	if v := d.Getenv(key); v != "" {
		return v
	}
	return def
}

func (f *auditFlags) s3Config(d Deps) audit.S3Config {
	return audit.S3Config{
		Endpoint: f.endpoint, Region: f.region, Bucket: f.bucket, PathStyle: f.pathStyle,
		AccessKey: d.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: d.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
}

func (f *auditFlags) requireChain() error {
	if f.chain == "" {
		return errors.New("--chain is required (or set LAST_AUDIT_CHAIN_ID; see ConfigMap last-gateway/last-audit-config)")
	}
	return nil
}

func newAuditCommand(d Deps) *cobra.Command {
	f := &auditFlags{}
	cmd := &cobra.Command{Use: "audit", Short: "Read and verify the write-once audit log"}
	pf := cmd.PersistentFlags()
	pf.StringVar(&f.endpoint, "endpoint", d.Getenv("LAST_AUDIT_ENDPOINT"), "S3 endpoint of the audit store (env LAST_AUDIT_ENDPOINT)")
	pf.StringVar(&f.bucket, "bucket", envOr(d, "LAST_AUDIT_BUCKET", "last-audit"), "audit bucket (env LAST_AUDIT_BUCKET)")
	pf.StringVar(&f.region, "region", envOr(d, "LAST_AUDIT_REGION", "us-east-1"), "S3 region (env LAST_AUDIT_REGION)")
	pf.StringVar(&f.chain, "chain", d.Getenv("LAST_AUDIT_CHAIN_ID"), "chain ID (env LAST_AUDIT_CHAIN_ID)")
	pf.BoolVar(&f.pathStyle, "path-style", true, "use path-style S3 addressing (required for RustFS)")
	cmd.AddCommand(newVerifyCommand(d, f), newLogCommand(d, f), newConformanceCommand(d, f))
	return cmd
}

func newVerifyCommand(d Deps, f *auditFlags) *cobra.Command {
	var pubkeyFile string
	var fromSeq uint64
	var expectHead int64
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify signatures, hash links, and sequence continuity of the audit chain",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := f.requireChain(); err != nil {
				return err
			}
			pemBytes, err := os.ReadFile(pubkeyFile)
			if err != nil {
				return err
			}
			pub, err := audit.ParsePublicKeyPEM(pemBytes)
			if err != nil {
				return err
			}
			sink, err := d.NewSink(cmd.Context(), f.s3Config(d))
			if err != nil {
				return err
			}
			opts := audit.VerifyOptions{FromSeq: fromSeq}
			if expectHead >= 0 {
				h := uint64(expectHead)
				opts.ExpectHeadAtLeast = &h
			}
			rep, err := audit.Verify(cmd.Context(), sink, pub, f.chain, opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if first, bad := rep.FirstBad(); bad {
				_, _ = fmt.Fprintf(out, "FAILED: first problem at seq %d (%s): %s\n", first.Seq, first.Kind, first.Detail)
				for _, p := range rep.Problems {
					_, _ = fmt.Fprintf(out, "  seq %-8d %-15s %s\n", p.Seq, p.Kind, p.Detail)
				}
				return ErrVerifyFailed
			}
			if rep.Head == nil {
				_, _ = fmt.Fprintln(out, "OK: chain is empty")
				return nil
			}
			_, _ = fmt.Fprintf(out, "OK: verified %d entries (seq %d..%d), head %s\n", rep.Entries, rep.First.Seq, rep.Head.Seq, rep.Head.Hash)
			return nil
		},
	}
	cmd.Flags().StringVar(&pubkeyFile, "pubkey-file", "", "PEM Ed25519 public key (ConfigMap last-gateway/last-audit-pubkey, key ed25519.pub)")
	_ = cmd.MarkFlagRequired("pubkey-file")
	cmd.Flags().Uint64Var(&fromSeq, "from-checkpoint", 0, "start verification at this seq (a checkpoint)")
	cmd.Flags().Int64Var(&expectHead, "expect-head", -1, "fail if the chain ends before this seq")
	return cmd
}

func newLogCommand(d Deps, f *auditFlags) *cobra.Command {
	var intent string
	var follow bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Print audit entries, optionally for one Intent",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := f.requireChain(); err != nil {
				return err
			}
			sink, err := d.NewSink(cmd.Context(), f.s3Config(d))
			if err != nil {
				return err
			}
			after := ""
			for {
				objs, err := sink.List(cmd.Context(), audit.EntryPrefix(f.chain), after)
				if err != nil {
					return err
				}
				for _, o := range objs {
					after = o.Key
					if len(o.VersionIDs) == 0 {
						continue
					}
					body, err := sink.Get(cmd.Context(), o.Key, o.VersionIDs[0])
					if err != nil {
						return err
					}
					var e audit.Entry
					if err := json.Unmarshal(body, &e); err != nil {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s  <malformed: %v>\n", o.Key, err)
						continue
					}
					if intent == "" || e.Intent == intent {
						printEntry(cmd.OutOrStdout(), e)
					}
				}
				if !follow {
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().StringVar(&intent, "intent", "", "only entries for this Intent (namespace/name)")
	cmd.Flags().BoolVar(&follow, "follow", false, "keep polling for new entries")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "poll interval with --follow")
	return cmd
}

func printEntry(w io.Writer, e audit.Entry) {
	payload := string(e.Payload)
	if len(payload) > 120 {
		payload = payload[:117] + "..."
	}
	_, _ = fmt.Fprintf(w, "%8d  %s  %-20s %-28s %-10s %s\n", e.Seq, e.TS, e.Type, e.Intent, e.Stage, payload)
}

func newConformanceCommand(d Deps, f *auditFlags) *cobra.Command {
	var retention time.Duration
	cmd := &cobra.Command{
		Use:   "conformance",
		Short: "Check that the audit bucket really enforces compliance-mode Object Lock",
		Long: "Writes one locked probe object under conformance/ and tries to delete, overwrite, and unlock it.\n" +
			"Runs with AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, and again with LAST_ADMIN_ACCESS_KEY/LAST_ADMIN_SECRET_KEY if set.\n" +
			"Results are reported per credential set; a set that is refused all writes (AccessDenied) passes most checks without proving the store enforces the lock, so run it with admin credentials too.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			type credSet struct{ name, ak, sk string }
			sets := []credSet{{"primary", d.Getenv("AWS_ACCESS_KEY_ID"), d.Getenv("AWS_SECRET_ACCESS_KEY")}}
			if ak := d.Getenv("LAST_ADMIN_ACCESS_KEY"); ak != "" {
				sets = append(sets, credSet{"admin", ak, d.Getenv("LAST_ADMIN_SECRET_KEY")})
			}
			out := cmd.OutOrStdout()
			failed := false
			for _, cs := range sets {
				cfg := f.s3Config(d)
				cfg.AccessKey, cfg.SecretKey = cs.ak, cs.sk
				client, err := d.NewS3Client(cmd.Context(), cfg)
				if err != nil {
					return err
				}
				results, err := audit.RunConformance(cmd.Context(), client, f.bucket, retention)
				if err != nil {
					_, _ = fmt.Fprintf(out, "[%s] SETUP FAILED: %v\n", cs.name, err)
					failed = true
					continue
				}
				for _, r := range results {
					status := "PASS"
					if !r.Passed {
						status, failed = "FAIL", true
					}
					_, _ = fmt.Fprintf(out, "[%s] %s  %-36s %s\n", cs.name, status, r.Name, r.Detail)
				}
			}
			if failed {
				return errors.New("WORM conformance failed")
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&retention, "retention", 24*time.Hour, "how long the probe object stays locked")
	return cmd
}
