package audit_test

import (
	"testing"

	"github.com/moogie-labs/last-operator/internal/audit"
)

func TestKeyLayout(t *testing.T) {
	if got := audit.EntryKey("c1", 42); got != "chains/c1/00000000000000000042.json" {
		t.Fatalf("EntryKey = %s", got)
	}
	if got := audit.CheckpointKey("c1", 7); got != "checkpoints/c1/00000000000000000007.json" {
		t.Fatalf("CheckpointKey = %s", got)
	}
	if audit.EntryPrefix("c1") != "chains/c1/" || audit.CheckpointPrefix("c1") != "checkpoints/c1/" {
		t.Fatal("prefixes wrong")
	}
}

func TestSeqFromKey(t *testing.T) {
	seq, err := audit.SeqFromKey(audit.EntryKey("c1", 1234))
	if err != nil || seq != 1234 {
		t.Fatalf("SeqFromKey = %d, %v", seq, err)
	}
	for _, bad := range []string{"chains/c1/12.json", "chains/c1/0000000000000000001x.json", "chains/c1/00000000000000000001.txt"} {
		if _, err := audit.SeqFromKey(bad); err == nil {
			t.Errorf("SeqFromKey(%q) accepted a malformed key", bad)
		}
	}
}
