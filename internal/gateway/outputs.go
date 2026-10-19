package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		if o.UID == mr.UID && o.Kind == "ManagedResource" && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
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
	existing, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "read secret: %v", err)
	case !isOutputSecretOf(existing, &mr, &intent):
		return nil, d.deny(codes.PermissionDenied, "secret %s/%s exists and is not %s's output secret", ns, secretName, mr.Name)
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
	desired.OwnerReferences = []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: "ManagedResource",
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
