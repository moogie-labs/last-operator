package audit

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

func EntryPrefix(chainID string) string      { return "chains/" + chainID + "/" }
func CheckpointPrefix(chainID string) string { return "checkpoints/" + chainID + "/" }

func EntryKey(chainID string, seq uint64) string {
	return fmt.Sprintf("%s%020d.json", EntryPrefix(chainID), seq)
}

func CheckpointKey(chainID string, seq uint64) string {
	return fmt.Sprintf("%s%020d.json", CheckpointPrefix(chainID), seq)
}

// SeqFromKey parses the zero-padded sequence number from an entry or checkpoint key.
func SeqFromKey(key string) (uint64, error) {
	digits, ok := strings.CutSuffix(path.Base(key), ".json")
	if !ok || len(digits) != 20 {
		return 0, fmt.Errorf("audit: malformed key %q", key)
	}
	seq, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("audit: malformed key %q: %w", key, err)
	}
	return seq, nil
}
