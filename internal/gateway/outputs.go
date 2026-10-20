package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

// validSecretKey is plan.ValidSecretKey: Validate applies the same rule
// before approval, so a valid plan never fails here for a key's shape.
func validSecretKey(k string) bool { return plan.ValidSecretKey(k) }

// outputWritten is the output.written payload; values are recorded only as hashes (spec §14.2).
type outputWritten struct {
	ManagedResource string            `json:"managedResource"`
	Secret          string            `json:"secret"`
	Values          map[string]string `json:"values"`
}

func ownedByIntent(mr *lastv1alpha1.ManagedResource, intent *lastv1alpha1.Intent) bool {
	if mr.Spec.IntentRef == nil || mr.Spec.IntentRef.Name != intent.Name {
		return false
	}
	for _, o := range mr.OwnerReferences {
		if o.UID == intent.UID && o.Kind == "Intent" && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

// kindManagedResource is the ownerReference kind of a ManagedResource.
const kindManagedResource = "ManagedResource"

// labelIntentUID binds an output Secret to the Intent whose phase wrote it.
const labelIntentUID = "last.moogie-labs.com/intent-uid"

// isOutputSecretOf reports whether the gateway wrote s as mr's output Secret for
// this Intent: the managed-by label, this Intent's UID label, and a controller
// ownerReference to mr. Anything else is foreign and is never overwritten.
func isOutputSecretOf(s *corev1.Secret, mr *lastv1alpha1.ManagedResource, intent *lastv1alpha1.Intent) bool {
	if s.Labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue || s.Labels[labelIntentUID] != string(intent.UID) {
		return false
	}
	for _, o := range s.OwnerReferences {
		if o.UID == mr.UID && o.Kind == kindManagedResource && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

// declaredFor reports whether the plan declares secret as the output Secret of
// the ManagedResource mr of the Intent: the resource at mr's position does.
func declaredFor(b plan.Body, intent, mr, secret string) bool {
	for i, r := range b.Resources {
		if lastv1alpha1.ManagedResourceName(intent, i) == mr {
			return r.OutputsSecretRef != nil && r.OutputsSecretRef.Name == secret
		}
	}
	return false
}

// heldBySibling reports whether s is an output Secret the gateway wrote for the
// Intent of intentUID that another ManagedResource the Intent controls holds.
// ManagedResource names are positional, so an Update that drops an earlier
// resource moves a kept one to another ManagedResource; its Secret may move
// with it, or deleting the old one would garbage-collect it. The holder is read
// live: an ownerReference alone proves nothing about what holds it now.
func (g *Gateway) heldBySibling(ctx context.Context, s *corev1.Secret, mr *lastv1alpha1.ManagedResource, intentUID types.UID) (bool, error) {
	if s.Labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue || s.Labels[labelIntentUID] != string(intentUID) {
		return false, nil
	}
	owner := metav1.GetControllerOf(s)
	if owner == nil || owner.Kind != kindManagedResource || owner.UID == mr.UID {
		return false, nil
	}
	var holder lastv1alpha1.ManagedResource
	err := g.crd.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: owner.Name}, &holder)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read managed resource %s: %w", owner.Name, err)
	}
	c := metav1.GetControllerOf(&holder)
	return holder.UID == owner.UID && c != nil && c.Kind == "Intent" && c.UID == intentUID, nil
}

// existingOutputSecret returns the Secret a write to mr's output Secret name
// replaces, or nil when there is none. It must be mr's own, or one another
// ManagedResource of the Intent holds that body declares for mr, which the
// write moves to mr by replacing its owners; any other is refused through d.
func (g *Gateway) existingOutputSecret(ctx context.Context, d denial, mr *lastv1alpha1.ManagedResource, intent *lastv1alpha1.Intent,
	body plan.Body, name string) (*corev1.Secret, error) {
	existing, err := g.kube.CoreV1().Secrets(mr.Namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "read secret: %v", err)
	case isOutputSecretOf(existing, mr, intent):
		return existing, nil
	}
	moves, err := g.heldBySibling(ctx, existing, mr, intent.UID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	if !moves || !declaredFor(body, intent.Name, mr.Name, name) {
		return nil, d.deny(codes.PermissionDenied, "secret %s/%s exists and is not %s's output secret", mr.Namespace, name, mr.Name)
	}
	return existing, nil
}

// plannedOutputs returns the outputs the approved plan declares for the output
// Secret name, and whether the plan declares that Secret at all.
func plannedOutputs(b plan.Body, name string) (map[string]string, bool) {
	for _, r := range b.Resources {
		if r.OutputsSecretRef != nil && r.OutputsSecretRef.Name == name {
			return r.Outputs, true
		}
	}
	return nil, false
}

func (g *Gateway) writeOutputSecret(ctx context.Context, req *gatewayv1.WriteOutputSecretRequest) (*gatewayv1.WriteOutputSecretResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	subject := req.GetSubjectHash()
	if !subjectPattern.MatchString(subject) {
		return nil, status.Error(codes.InvalidArgument, "subject_hash must be sha256:<64 lowercase hex digits>")
	}
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, subject: subject, rpc: "WriteOutputSecret"}
	p := g.phases.get(key)
	if p == nil || p.subject != subject {
		return nil, d.deny(codes.FailedPrecondition, "no open phase for this plan")
	}
	d.stage = string(p.stage)
	if !p.mu.TryLock() {
		return nil, status.Error(codes.Aborted, "a step of this phase is already running")
	}
	defer p.mu.Unlock()
	if p.closed {
		return nil, d.deny(codes.FailedPrecondition, "phase is closed")
	}
	// A Remediate plan's outputs are the executed plan's, naming its steps: the
	// repair's own step results must never stand in for them.
	if p.stage == lastv1alpha1.StageRemediate {
		return nil, d.deny(codes.FailedPrecondition, "a Remediate phase writes no output Secrets; the executed plan's outputs stand")
	}
	if req.GetManagedResource() == "" || len(req.GetValues()) == 0 {
		return nil, d.deny(codes.InvalidArgument, "managed_resource and values are required")
	}

	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	var mr lastv1alpha1.ManagedResource
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: req.GetManagedResource()}, &mr); err != nil {
		return nil, notFoundOr(err, "managed resource")
	}
	if !ownedByIntent(&mr, &intent) {
		return nil, d.deny(codes.PermissionDenied, "managed resource %s is not owned by intent %s", mr.Name, key)
	}
	if mr.Spec.OutputsSecretRef == nil {
		return nil, d.deny(codes.InvalidArgument, "managed resource %s has no outputsSecretRef", mr.Name)
	}
	secretName := mr.Spec.OutputsSecretRef.Name
	// The operator can edit a ManagedResource, so its Secret name is trusted only
	// when the approved plan declares it.
	planned, ok := plannedOutputs(p.body, secretName)
	if !ok {
		return nil, d.deny(codes.PermissionDenied, "secret %s is not an output secret of the approved plan", secretName)
	}
	// The operator sends the plan's outputs; any other value was not approved.
	if !maps.Equal(planned, req.GetValues()) {
		return nil, d.deny(codes.PermissionDenied, "values for secret %s differ from the approved plan's outputs", secretName)
	}

	data := make(map[string][]byte, len(req.GetValues()))
	hashes := make(map[string]string, len(req.GetValues()))
	for k, expr := range req.GetValues() {
		if !validSecretKey(k) {
			return nil, d.deny(codes.InvalidArgument, "%q is not a valid Secret key", truncateReason(k))
		}
		v, err := p.results.Value(expr)
		if err != nil {
			// The reason never repeats the expression or the error: a mistaken literal could be a credential.
			return nil, d.deny(codes.FailedPrecondition, "value for key %q is not a resolvable step reference", k)
		}
		sum := sha256.Sum256([]byte(v))
		data[k], hashes[k] = []byte(v), "sha256:"+hex.EncodeToString(sum[:])
	}

	secrets := g.kube.CoreV1().Secrets(ns)
	existing, err := g.existingOutputSecret(ctx, d, &mr, &intent, p.body, secretName)
	if err != nil {
		return nil, err
	}
	if _, err := w.Append(ctx, audit.Draft{Type: audit.TypeOutputWritten, Actor: actor, Intent: key, Stage: string(p.stage),
		SubjectHash: p.subject, AuthorizedBy: p.auth,
		Payload: outputWritten{ManagedResource: mr.Name, Secret: secretName, Values: hashes}}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "record output.written: %v", err)
	}
	// Data is replaced wholesale: keys absent from this write are removed. An
	// existing Secret keeps its annotations and any other labels.
	desired := &corev1.Secret{Type: corev1.SecretTypeOpaque}
	if existing != nil {
		desired = existing.DeepCopy()
	}
	desired.Name, desired.Namespace, desired.Data = secretName, ns, data
	if desired.Labels == nil {
		desired.Labels = map[string]string{}
	}
	desired.Labels[sandbox.LabelManagedBy] = sandbox.ManagedByValue
	desired.Labels[labelIntentUID] = string(intent.UID)
	desired.OwnerReferences = []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: kindManagedResource,
		Name: mr.Name, UID: mr.UID, Controller: new(true)}}
	if existing == nil {
		_, err = secrets.Create(ctx, desired, metav1.CreateOptions{})
	} else {
		_, err = secrets.Update(ctx, desired, metav1.UpdateOptions{})
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		return nil, status.Error(codes.Aborted, "output secret changed concurrently; retry")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "write output secret: %v", err)
	}
	return &gatewayv1.WriteOutputSecretResponse{SecretName: secretName}, nil
}

// outputMoved is the output.moved payload: names only, never the Secret's data.
type outputMoved struct {
	Secret string `json:"secret"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// moveKeptOutputSecrets moves each output Secret the executed plan declares to
// the ManagedResource that now declares it, when another ManagedResource the
// Intent of uid controls still holds it (heldBySibling). An Update that drops an
// earlier resource and keeps a later one without new outputs would otherwise
// lose that Secret when the old ManagedResource is deleted. The data is left
// as it is; each move is audited as output.moved from the entry template
// before the Secret is written. The caller holds the Intent's lock.
func (g *Gateway) moveKeptOutputSecrets(ctx context.Context, w *audit.Writer, entry audit.Draft, ns, intent string, uid types.UID,
	resources []lastv1alpha1.ManagedResourceSpec) error {
	for i, r := range resources {
		if r.OutputsSecretRef == nil {
			continue
		}
		var mr lastv1alpha1.ManagedResource
		err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: lastv1alpha1.ManagedResourceName(intent, i)}, &mr)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read managed resource %s: %w", lastv1alpha1.ManagedResourceName(intent, i), err)
		}
		if c := metav1.GetControllerOf(&mr); c == nil || c.UID != uid {
			continue
		}
		secrets := g.kube.CoreV1().Secrets(ns)
		s, err := secrets.Get(ctx, r.OutputsSecretRef.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read secret %s: %w", r.OutputsSecretRef.Name, err)
		}
		moves, err := g.heldBySibling(ctx, s, &mr, uid)
		if err != nil {
			return err
		}
		if !moves {
			continue
		}
		from := metav1.GetControllerOf(s).Name
		e := entry
		e.Type, e.Payload = audit.TypeOutputMoved, outputMoved{Secret: s.Name, From: from, To: mr.Name}
		if _, err := w.Append(ctx, e); err != nil {
			return fmt.Errorf("record %s: %w", audit.TypeOutputMoved, err)
		}
		s.OwnerReferences = []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: kindManagedResource,
			Name: mr.Name, UID: mr.UID, Controller: new(true)}}
		if _, err := secrets.Update(ctx, s, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("move secret %s: %w", s.Name, err)
		}
		slog.Info("Moved output Secret to the ManagedResource that now declares it", "intent", entry.Intent, "secret", s.Name,
			"from", from, "to", mr.Name)
	}
	return nil
}
