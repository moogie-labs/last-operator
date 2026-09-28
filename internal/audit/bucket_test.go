package audit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/moogie-labs/last-operator/internal/audit"
)

// TestEnsureLockedBucketRejectsNonPositiveRetention checks that the retentionDays
// validation happens before any client call: a nil client must not panic.
func TestEnsureLockedBucketRejectsNonPositiveRetention(t *testing.T) {
	for _, days := range []int32{0, -1} {
		err := audit.EnsureLockedBucket(context.Background(), nil, "bucket", days)
		if err == nil || !strings.Contains(err.Error(), "retentionDays") {
			t.Fatalf("EnsureLockedBucket(retentionDays=%d) = %v, want an error mentioning retentionDays", days, err)
		}
	}
}
