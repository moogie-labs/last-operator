package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	stateKey        = "record.json"
	labelComponent  = "app.kubernetes.io/component"
	stateComponent  = "intent-state"
	maxUsedSubjects = 50
)

// MaxRecordBytes bounds an encoded IntentRecord; a ConfigMap holds at most 1 MiB.
const MaxRecordBytes = 900 << 10

// IntentRecord is the gateway's own record of an Intent (spec §8.3): the open
// phase, the last successfully executed plan, and the approval subjects that
// already opened a phase. Only the leading gateway writes it, and only after the
// matching phase.begin / phase.end entry is durable; the operator cannot read it.
type IntentRecord struct {
	Intent   string        `json:"intent"`
	Open     *OpenPhase    `json:"open,omitempty"`
	Executed *ExecutedPlan `json:"executed,omitempty"`
	Used     []string      `json:"used,omitempty"`
}

// OpenPhase is a phase.begin without a phase.end yet.
type OpenPhase struct {
	Stage       string `json:"stage"`
	SubjectHash string `json:"subjectHash"`
	BeginSeq    uint64 `json:"beginSeq"`
	Epoch       string `json:"epoch"`
}

// ExecutedPlan is the last plan whose phase ended successfully.
type ExecutedPlan struct {
	Stage    string `json:"stage"`
	PlanHash string `json:"planHash"`
	Plan     string `json:"plan"` // canonical JSON of plan.Body
	EndSeq   uint64 `json:"endSeq"`
}

// MarkUsed records that subject opened a phase; a subject opens at most one.
func (r *IntentRecord) MarkUsed(subject string) {
	if slices.Contains(r.Used, subject) {
		return
	}
	r.Used = append(r.Used, subject)
	if len(r.Used) > maxUsedSubjects {
		r.Used = r.Used[len(r.Used)-maxUsedSubjects:]
	}
}

// encodeRecord returns the record's JSON without HTML escaping (a plan full of
// <, > and & would otherwise grow sixfold) and without a trailing newline. It
// fails when the result would not fit a ConfigMap.
func encodeRecord(r IntentRecord) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("gateway: encode intent record: %w", err)
	}
	body := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(body) > MaxRecordBytes {
		return nil, fmt.Errorf("gateway: intent record is %d bytes; the limit is %d (too large)", len(body), MaxRecordBytes)
	}
	return body, nil
}

// StateStore keeps IntentRecords in ConfigMaps in the gateway namespace.
type StateStore struct {
	kube kubernetes.Interface
	ns   string
}

func NewStateStore(kube kubernetes.Interface, namespace string) *StateStore {
	return &StateStore{kube: kube, ns: namespace}
}

// LoadedRecord is a record plus the ConfigMap it came from (nil when new).
type LoadedRecord struct {
	IntentRecord
	cm *corev1.ConfigMap
}

func recordName(intent string) string {
	sum := sha256.Sum256([]byte(intent))
	return "last-intent-" + hex.EncodeToString(sum[:])[:20]
}

// Load returns the intent's record, or an empty one when none exists.
func (s *StateStore) Load(ctx context.Context, intent string) (*LoadedRecord, error) {
	cm, err := s.kube.CoreV1().ConfigMaps(s.ns).Get(ctx, recordName(intent), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &LoadedRecord{IntentRecord: IntentRecord{Intent: intent}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gateway: load intent record: %w", err)
	}
	var rec IntentRecord
	if err := json.Unmarshal([]byte(cm.Data[stateKey]), &rec); err != nil {
		return nil, fmt.Errorf("gateway: intent record %s is corrupt: %w", cm.Name, err)
	}
	if rec.Intent != intent {
		return nil, fmt.Errorf("gateway: intent record %s belongs to %q, not %q", cm.Name, rec.Intent, intent)
	}
	return &LoadedRecord{IntentRecord: rec, cm: cm}, nil
}

// Save writes the record, creating its ConfigMap on first use. An update carries
// the loaded resourceVersion, so a concurrent writer fails instead of overwriting.
func (s *StateStore) Save(ctx context.Context, r *LoadedRecord) error {
	body, err := encodeRecord(r.IntentRecord)
	if err != nil {
		return err
	}
	cms := s.kube.CoreV1().ConfigMaps(s.ns)
	if r.cm == nil {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: recordName(r.Intent), Namespace: s.ns,
				Labels: map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, labelComponent: stateComponent}},
			Data: map[string]string{stateKey: string(body)},
		}
		created, err := cms.Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("gateway: create intent record: %w", err)
		}
		r.cm = created
		return nil
	}
	cm := r.cm.DeepCopy()
	cm.Data = map[string]string{stateKey: string(body)}
	updated, err := cms.Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("gateway: update intent record: %w", err)
	}
	r.cm = updated
	return nil
}
