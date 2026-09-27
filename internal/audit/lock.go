package audit

import (
	"errors"
	"fmt"
	"time"
)

// CheckLockConfig enforces the gateway's startup requirement (spec §14.4): Object
// Lock enabled, compliance mode, and default retention at least as long as configured.
func CheckLockConfig(lc LockConfig, retention time.Duration) error {
	switch {
	case !lc.Enabled:
		return errors.New("audit: bucket does not have Object Lock enabled")
	case lc.Mode != "COMPLIANCE":
		return fmt.Errorf("audit: bucket default retention mode is %q, want COMPLIANCE", lc.Mode)
	case lc.DefaultRetention < retention:
		return fmt.Errorf("audit: bucket default retention %s is shorter than configured %s", lc.DefaultRetention, retention)
	}
	return nil
}
