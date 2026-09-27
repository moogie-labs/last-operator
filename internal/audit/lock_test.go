package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
)

func TestCheckLockConfig(t *testing.T) {
	year := 365 * 24 * time.Hour
	cases := []struct {
		name string
		lc   audit.LockConfig
		want string // substring of the error; "" = ok
	}{
		{"compliant", audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: year}, ""},
		{"longer is fine", audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: 2 * year}, ""},
		{"not enabled", audit.LockConfig{}, "Object Lock"},
		{"governance", audit.LockConfig{Enabled: true, Mode: "GOVERNANCE", DefaultRetention: year}, "COMPLIANCE"},
		{"too short", audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: 24 * time.Hour}, "shorter"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := audit.CheckLockConfig(c.lc, year)
			if c.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}
