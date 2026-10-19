package gateway

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	stateKey        = "record.json"
	labelComponent  = "app.kubernetes.io/component"
	stateComponent  = "intent-state"
	maxUsedSubjects = 50
	// Discovery subjects have their own, larger list so they never evict a plan subject.
	maxUsedDiscoverySubjects = 200
)

// maxIndexEntries bounds an Intent's QueryAudit index (M3 spec §3.5).
const maxIndexEntries = 200

// IndexEntry is one audit entry QueryAudit can serve for the Intent.
type IndexEntry struct {
	Seq  uint64 `json:"seq"`
	Type string `json:"type"`
}

// errRecordTooLarge means a record would not fit its ConfigMap.
var errRecordTooLarge = errors.New("gateway: intent record too large")

// MaxRecordBytes bounds an encoded IntentRecord; a ConfigMap holds at most 1 MiB.
const MaxRecordBytes = 900 << 10

// IntentRecord is the gateway's own record of an Intent (spec §8.3): the open
// phase, the last successfully executed plan, and the approval subjects that
// already opened a phase. Only the leading gateway writes it, and only after the
// matching phase.begin / phase.end entry is durable; the operator cannot read it.
type IntentRecord struct {
	Intent string `json:"intent"`
	// IntentUID is the UID of the Intent object this record belongs to. A record
	// for another UID (the Intent was deleted and recreated) is reset on Load.
	IntentUID string        `json:"intentUID,omitempty"`
	Open      *OpenPhase    `json:"open,omitempty"`
	Executed  *ExecutedPlan `json:"executed,omitempty"`
	Used      []string      `json:"used,omitempty"`
	// UsedDiscovery holds Discovery subjects, kept apart from Used.
	UsedDiscovery []string `json:"usedDiscovery,omitempty"`
	// Index lists the Intent's recent indexed audit entries, oldest first.
	Index []IndexEntry `json:"index,omitempty"`
	// IndexFloor is the highest seq ever dropped from Index; QueryAudit reports
	// truncation for cursors below it.
	IndexFloor uint64 `json:"indexFloor,omitempty"`
}

// OpenPhase is a phase.begin without a phase.end yet.
type OpenPhase struct {
	Stage       string `json:"stage"`
	SubjectHash string `json:"subjectHash"`
	BeginSeq    uint64 `json:"beginSeq"`
	Epoch       string `json:"epoch"`
	// Auth is the authorization phase.begin recorded, so an interrupted phase.end can carry it.
	Auth *audit.Authorization `json:"auth,omitempty"`
}

// ExecutedPlan is the last plan whose phase ended successfully.
type ExecutedPlan struct {
	Stage    string `json:"stage"`
	PlanHash string `json:"planHash"`
	Plan     string `json:"plan"` // canonical JSON of plan.Body
	EndSeq   uint64 `json:"endSeq"`
}

// MarkUsed records that subject opened a phase. Discovery subjects go to
// UsedDiscovery (newest 200), every other stage to Used (newest 50).
func (r *IntentRecord) MarkUsed(stage lastv1alpha1.Stage, subject string) {
	list, limit := &r.Used, maxUsedSubjects
	if stage == lastv1alpha1.StageDiscovery {
		list, limit = &r.UsedDiscovery, maxUsedDiscoverySubjects
	}
	if slices.Contains(*list, subject) {
		return
	}
	*list = append(*list, subject)
	if len(*list) > limit {
		*list = (*list)[len(*list)-limit:]
	}
}

// WasUsed reports whether subject already opened a phase and is still remembered.
func (r *IntentRecord) WasUsed(subject string) bool {
	return slices.Contains(r.Used, subject) || slices.Contains(r.UsedDiscovery, subject)
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
		return nil, fmt.Errorf("%w: %d bytes; the limit is %d", errRecordTooLarge, len(body), MaxRecordBytes)
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

// Load returns the intent's record for the Intent object with this UID, or an
// empty one when none exists. A stored record for a different, non-empty UID
// comes back empty (the Intent was recreated); its ConfigMap is kept so the next
// Save overwrites it. A record with no stored UID adopts uid.
func (s *StateStore) Load(ctx context.Context, intent, uid string) (*LoadedRecord, error) {
	r, err := s.LoadAny(ctx, intent)
	if err != nil {
		return nil, err
	}
	if uid == "" {
		return r, nil
	}
	if r.IntentUID != "" && r.IntentUID != uid {
		r.IntentRecord = IntentRecord{Intent: intent}
	}
	r.IntentUID = uid
	return r, nil
}

// LoadAny returns the intent's record whatever UID it holds, keeping the stored
// UID. A new record has an empty UID, which the next Load adopts.
func (s *StateStore) LoadAny(ctx context.Context, intent string) (*LoadedRecord, error) {
	cm, err := s.kube.CoreV1().ConfigMaps(s.ns).Get(ctx, recordName(intent), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &LoadedRecord{IntentRecord: IntentRecord{Intent: intent}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gateway: load intent record: %w", err)
	}
	return decodeRecord(cm, intent)
}

func decodeRecord(cm *corev1.ConfigMap, intent string) (*LoadedRecord, error) {
	var rec IntentRecord
	if err := json.Unmarshal([]byte(cm.Data[stateKey]), &rec); err != nil {
		return nil, fmt.Errorf("gateway: intent record %s is corrupt: %w", cm.Name, err)
	}
	if intent != "" && rec.Intent != intent {
		return nil, fmt.Errorf("gateway: intent record %s belongs to %q, not %q", cm.Name, rec.Intent, intent)
	}
	return &LoadedRecord{IntentRecord: rec, cm: cm}, nil
}

// Delete removes the intent's record; a missing record is not an error.
func (s *StateStore) Delete(ctx context.Context, intent string) error {
	err := s.kube.CoreV1().ConfigMaps(s.ns).Delete(ctx, recordName(intent), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("gateway: delete intent record: %w", err)
	}
	return nil
}

// List returns every intent record in the gateway namespace.
func (s *StateStore) List(ctx context.Context) ([]*LoadedRecord, error) {
	l, err := s.kube.CoreV1().ConfigMaps(s.ns).List(ctx, metav1.ListOptions{LabelSelector: labelComponent + "=" + stateComponent})
	if err != nil {
		return nil, fmt.Errorf("gateway: list intent records: %w", err)
	}
	out := make([]*LoadedRecord, 0, len(l.Items))
	for i := range l.Items {
		r, err := decodeRecord(&l.Items[i], "")
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
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

// AddIndex merges entries into the index, keeping it sorted, unique by seq,
// and at most maxIndexEntries long.
func (r *IntentRecord) AddIndex(es []IndexEntry) {
	r.Index = append(r.Index, es...)
	slices.SortFunc(r.Index, func(a, b IndexEntry) int { return cmp.Compare(a.Seq, b.Seq) })
	r.Index = slices.CompactFunc(r.Index, func(a, b IndexEntry) bool { return a.Seq == b.Seq })
	if n := len(r.Index) - maxIndexEntries; n > 0 {
		r.DropIndex(n)
	}
}

// DropIndex drops the n oldest index entries and raises IndexFloor past them.
func (r *IntentRecord) DropIndex(n int) {
	n = min(n, len(r.Index))
	if n == 0 {
		return
	}
	r.IndexFloor = max(r.IndexFloor, r.Index[n-1].Seq)
	r.Index = slices.Clone(r.Index[n:])
}
