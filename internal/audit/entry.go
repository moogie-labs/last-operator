// Package audit implements the write-once, hash-chained audit log (spec §14).
package audit

import "encoding/json"

// Entry types (spec §14.2).
const (
	TypeGatewayStart      = "gateway.start"
	TypeCheckpoint        = "checkpoint"
	TypePhaseBegin        = "phase.begin"
	TypePhaseEnd          = "phase.end"
	TypeLLMRequest        = "llm.request"
	TypeLLMResponse       = "llm.response"
	TypeCommandStart      = "command.start"
	TypeCommandResult     = "command.result"
	TypeCredentialIssued  = "credential.issued"
	TypeEgressGranted     = "egress.granted"
	TypeEgressRevoked     = "egress.revoked"
	TypeProbeStart        = "probe.start"
	TypeProbeResult       = "probe.result"
	TypeImageBuildStart   = "image.build.start"
	TypeImageBuildResult  = "image.build.result"
	TypeImageDeleted      = "image.deleted"
	TypeImageGCStart      = "image.gc.start"
	TypeImageGCResult     = "image.gc.result"
	TypeApprovalObserved  = "approval.observed"
	TypeRuleObserved      = "rule.observed"
	TypeSuggestionDecided = "suggestion.decided"
	TypePolicyDenied      = "policy.denied"
	// output Secret written; values recorded as SHA-256 (spec §15)
	TypeOutputWritten = "output.written"
	// a ManagedResource a succeeded Update dropped, deleted by the gateway; its
	// output Secret goes with it by garbage collection (spec §15)
	TypeManagedResourceDeleted = "managedresource.deleted"
)

// GenesisHash is the prevHash of the entry at seq 0.
const GenesisHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// TimeFormat is fixed-width so an entry's bytes never depend on trailing-zero trimming.
const TimeFormat = "2006-01-02T15:04:05.000000000Z"

// Authorization records what allowed an action.
type Authorization struct {
	Kind string `json:"kind"` // approval | rule | auto
	Name string `json:"name,omitempty"`
}

// EntryVersion is the entry format version this package writes and verifies.
const EntryVersion = 1

// Entry is one signed link in the audit chain. It is stored as its RFC 8785
// canonical JSON encoding (spec §14.2); V is covered by the hash.
type Entry struct {
	V            int             `json:"v"`
	Seq          uint64          `json:"seq"`
	ChainID      string          `json:"chainId"`
	PrevHash     string          `json:"prevHash"`
	TS           string          `json:"ts"`
	Type         string          `json:"type"`
	Actor        string          `json:"actor"`
	Intent       string          `json:"intent,omitempty"`
	Stage        string          `json:"stage,omitempty"`
	SubjectHash  string          `json:"subjectHash,omitempty"`
	AuthorizedBy *Authorization  `json:"authorizedBy,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Hash         string          `json:"hash,omitempty"`
	Sig          string          `json:"sig,omitempty"`
}

// Draft is what callers supply; the Writer fills in the chain fields.
type Draft struct {
	Type         string
	Actor        string
	Intent       string
	Stage        string
	SubjectHash  string
	AuthorizedBy *Authorization
	Payload      any
}

// CheckpointPayload is the payload of a checkpoint entry.
type CheckpointPayload struct {
	PrevSeq  uint64 `json:"prevSeq"`
	PrevHash string `json:"prevHash"`
}

// CheckpointIndex is the body of checkpoints/<chainId>/<seq>.json.
type CheckpointIndex struct {
	Seq  uint64 `json:"seq"`
	Hash string `json:"hash"`
}
