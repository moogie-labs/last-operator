package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	outputValue  = "s3cr3t-value-123"
	outputSecret = "web-db-outputs"
	outputMR     = "web-db"
	passwordKey  = "password"
	passwordRef  = "${steps.s1.json.password}"
)

func managedResource(ownerUID types.UID) *lastv1alpha1.ManagedResource {
	return &lastv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{Name: outputMR, Namespace: envNS, UID: "mr-uid",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: "Intent",
				Name: envIntent, UID: ownerUID, Controller: new(true)}}},
		Spec: lastv1alpha1.ManagedResourceSpec{IntentRef: &lastv1alpha1.LocalRef{Name: envIntent},
			OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outputSecret}},
	}
}

func (e *testEnv) writeOutputs(subject, mr string, values map[string]string) (*gatewayv1.WriteOutputSecretResponse, error) {
	return e.g.writeOutputSecret(e.ctx, &gatewayv1.WriteOutputSecretRequest{Intent: ref(), SubjectHash: subject, ManagedResource: mr, Values: values})
}

func newOutputEnv(t *testing.T, ownerUID types.UID) (*testEnv, string) {
	t.Helper()
	return newOutputEnvWith(t, ownerUID, map[string]string{passwordKey: passwordRef})
}

// newOutputEnvWith runs a two-step plan whose one resource declares outputs.
func newOutputEnvWith(t *testing.T, ownerUID types.UID, outputs map[string]string) (*testEnv, string) {
	t.Helper()
	env, in := newRunEnv(t, twoSteps(), lastv1alpha1.ManagedResourceSpec{
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outputSecret}, Outputs: outputs})
	if err := env.crd.Create(context.Background(), managedResource(ownerUID)); err != nil {
		t.Fatal(err)
	}
	if _, ok := runAll(t, env, in)["s1"]; !ok {
		t.Fatal("runAll returned no result for s1")
	}
	return env, in.Status.Plan.Hash
}

func TestWriteOutputSecretWritesHashedAuditAndOwnedSecret(t *testing.T) {
	env, h := newOutputEnv(t, envIntentUID)
	resp, err := env.writeOutputs(h, outputMR, map[string]string{passwordKey: passwordRef})
	if err != nil || resp.GetSecretName() != outputSecret {
		t.Fatalf("WriteOutputSecret = %v, %v", resp, err)
	}
	s, err := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), outputSecret, metav1.GetOptions{})
	if err != nil || string(s.Data[passwordKey]) != outputValue || s.Labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue ||
		len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "mr-uid" || s.OwnerReferences[0].Kind != kindManagedResource {
		t.Fatalf("secret = %+v, %v", s, err)
	}
	last := env.last()
	if last.Type != audit.TypeOutputWritten || last.Actor != testOperator || last.AuthorizedBy == nil {
		t.Fatalf("last entry = %+v", last)
	}
	var p outputWritten
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(outputValue))
	if p.Values[passwordKey] != "sha256:"+hex.EncodeToString(sum[:]) || p.Secret != outputSecret || p.ManagedResource != outputMR {
		t.Fatalf("output.written payload = %+v", p)
	}
	for _, e := range env.entries() {
		if e.Type != audit.TypeCommandResult && strings.Contains(string(e.Payload), outputValue) {
			t.Fatalf("entry %s carries the output value", e.Type)
		}
	}
}

func TestWriteOutputSecretRefusals(t *testing.T) {
	t.Run("managed resource of another intent", func(t *testing.T) {
		env, h := newOutputEnv(t, "someone-else")
		_, err := env.writeOutputs(h, outputMR, map[string]string{passwordKey: passwordRef})
		env.wantDenied(err, codes.PermissionDenied)
	})
	t.Run("existing user secret is never overwritten", func(t *testing.T) {
		env, h := newOutputEnv(t, envIntentUID)
		mine := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: outputSecret, Namespace: envNS}, Data: map[string][]byte{"k": []byte("user")}}
		if _, err := env.kube.CoreV1().Secrets(envNS).Create(context.Background(), mine, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		_, err := env.writeOutputs(h, outputMR, map[string]string{passwordKey: passwordRef})
		env.wantDenied(err, codes.PermissionDenied)
		s, _ := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), outputSecret, metav1.GetOptions{})
		if string(s.Data["k"]) != "user" || s.Data[passwordKey] != nil {
			t.Fatalf("the user's secret was modified: %+v", s.Data)
		}
	})
	t.Run("own secret is updated", func(t *testing.T) {
		env, h := newOutputEnv(t, envIntentUID)
		values := map[string]string{passwordKey: passwordRef}
		if _, err := env.writeOutputs(h, outputMR, values); err != nil {
			t.Fatal(err)
		}
		if _, err := env.writeOutputs(h, outputMR, values); err != nil {
			t.Fatalf("second write = %v", err)
		}
		if got := env.secret(outputSecret); string(got.Data[passwordKey]) != outputValue {
			t.Fatalf("data = %v", got.Data)
		}
	})
	t.Run("values must be the approved plan's outputs", func(t *testing.T) {
		env, h := newOutputEnv(t, envIntentUID)
		cases := map[string]struct {
			values map[string]string
			code   codes.Code
		}{
			"literal":     {map[string]string{passwordKey: "hunter2"}, codes.PermissionDenied},
			"other ref":   {map[string]string{passwordKey: "${steps.s1.stdout}"}, codes.PermissionDenied},
			"extra key":   {map[string]string{passwordKey: passwordRef, "name": "${steps.s1.json.metadata.name}"}, codes.PermissionDenied},
			"missing key": {map[string]string{"absent": passwordRef}, codes.PermissionDenied},
			"no values":   {map[string]string{}, codes.InvalidArgument},
		}
		for name, tc := range cases {
			_, err := env.writeOutputs(h, outputMR, tc.values)
			if status.Code(err) != tc.code || env.last().Type != audit.TypePolicyDenied {
				t.Errorf("%s: %v (last %s), want %v with policy.denied", name, err, env.last().Type, tc.code)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(string(env.last().Payload), "hunter2") {
				t.Errorf("%s: the literal leaked into the error or audit entry", name)
			}
		}
		_, err := env.writeOutputs("sha256:"+strings.Repeat("0", 64), outputMR, map[string]string{passwordKey: passwordRef})
		env.wantDenied(err, codes.FailedPrecondition)
		if _, gerr := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), outputSecret, metav1.GetOptions{}); gerr == nil {
			t.Fatal("a refused write created the secret")
		}
	})
	t.Run("an unresolvable or invalid planned output is refused", func(t *testing.T) {
		cases := map[string]struct {
			outputs map[string]string
			code    codes.Code
		}{
			"missing json key": {map[string]string{"p": "${steps.s1.json.nope}"}, codes.FailedPrecondition},
		}
		for name, tc := range cases {
			env, h := newOutputEnvWith(t, envIntentUID, tc.outputs)
			_, err := env.writeOutputs(h, outputMR, tc.outputs)
			if status.Code(err) != tc.code || env.last().Type != audit.TypePolicyDenied {
				t.Errorf("%s: %v (last %s), want %v with policy.denied", name, err, env.last().Type, tc.code)
			}
		}
	})
	t.Run("a planned output with an invalid Secret key never opens a phase", func(t *testing.T) {
		env, in := newRunEnv(t, twoSteps(), lastv1alpha1.ManagedResourceSpec{
			OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outputSecret}, Outputs: map[string]string{"pass word": passwordRef}})
		_, err := env.begin(in.Status.Plan.Hash)
		env.wantDenied(err, codes.InvalidArgument)
	})
	t.Run("the output write re-checks Secret keys", func(t *testing.T) {
		// plan.Validate refuses such a plan at BeginPhase; the write keeps its
		// own check in case an approved body ever bypasses validation.
		env, h := newOutputEnv(t, envIntentUID)
		bad := map[string]string{"pass word": passwordRef}
		env.g.phases.get(envKey).body.Resources[0].Outputs = bad
		_, err := env.writeOutputs(h, outputMR, bad)
		env.wantDenied(err, codes.InvalidArgument)
	})
}

func TestWriteOutputSecretMalformedSubjectWritesNothing(t *testing.T) {
	env, _ := newOutputEnv(t, envIntentUID)
	before := len(env.entries())
	_, err := env.writeOutputs("not-a-hash", outputMR, map[string]string{"p": "${steps.s1.stdout}"})
	if status.Code(err) != codes.InvalidArgument || len(env.entries()) != before {
		t.Fatalf("err = %v, entries %d -> %d", err, before, len(env.entries()))
	}
}

func TestWriteOutputSecretClosedPhaseAndConcurrency(t *testing.T) {
	env, h := newOutputEnv(t, envIntentUID)
	values := map[string]string{passwordKey: passwordRef}
	p := env.g.phases.get(envKey)
	p.mu.Lock()
	_, err := env.writeOutputs(h, outputMR, values)
	p.mu.Unlock()
	if status.Code(err) != codes.Aborted {
		t.Fatalf("concurrent write = %v, want Aborted", err)
	}
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	_, err = env.writeOutputs(h, outputMR, values)
	env.wantDenied(err, codes.FailedPrecondition)
	if _, gerr := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), outputSecret, metav1.GetOptions{}); gerr == nil {
		t.Fatal("a closed phase wrote the secret")
	}
}

func TestWriteOutputSecretMissingObjectsAndRefs(t *testing.T) {
	env, h := newOutputEnv(t, envIntentUID)
	values := map[string]string{passwordKey: passwordRef}
	before := len(env.entries())
	if _, err := env.writeOutputs(h, "nope", values); status.Code(err) != codes.NotFound || len(env.entries()) != before {
		t.Fatalf("missing managed resource = %v", err)
	}
	mr := managedResource(envIntentUID)
	mr.Name, mr.UID, mr.Spec.OutputsSecretRef = "bare", "bare-uid", nil
	if err := env.crd.Create(context.Background(), mr); err != nil {
		t.Fatal(err)
	}
	_, err := env.writeOutputs(h, "bare", values)
	env.wantDenied(err, codes.InvalidArgument)
}

func TestWriteOutputSecretAuditPrecedesSecret(t *testing.T) {
	env, h := newOutputEnv(t, envIntentUID)
	var auditedAtCreate bool
	n := len(env.entries())
	env.kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret); s.Name == outputSecret {
			auditedAtCreate = len(env.entries()) == n+1 && env.last().Type == audit.TypeOutputWritten
		}
		return false, nil, nil
	})
	if _, err := env.writeOutputs(h, outputMR, map[string]string{passwordKey: passwordRef}); err != nil {
		t.Fatal(err)
	}
	if !auditedAtCreate {
		t.Fatal("output.written was not durable before the Secret was created")
	}
}

func (e *testEnv) secret(name string) *corev1.Secret {
	e.t.Helper()
	s, err := e.kube.CoreV1().Secrets(envNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func ownerRef(uid types.UID, controller bool) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: kindManagedResource, Name: "x", UID: uid, Controller: &controller}
}

func TestWriteOutputSecretRefusesNameOutsideTheApprovedPlan(t *testing.T) {
	env, h := newOutputEnv(t, envIntentUID)
	var mr lastv1alpha1.ManagedResource
	if err := env.crd.Get(context.Background(), client.ObjectKey{Namespace: envNS, Name: outputMR}, &mr); err != nil {
		t.Fatal(err)
	}
	mr.Spec.OutputsSecretRef.Name = "someone-elses"
	if err := env.crd.Update(context.Background(), &mr); err != nil {
		t.Fatal(err)
	}
	_, err := env.writeOutputs(h, outputMR, map[string]string{passwordKey: passwordRef})
	env.wantDenied(err, codes.PermissionDenied)
	if _, gerr := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), "someone-elses", metav1.GetOptions{}); gerr == nil {
		t.Fatal("a Secret outside the approved plan was written")
	}
}

func TestWriteOutputSecretRefusesAReparentedManagedResource(t *testing.T) {
	env, h := newOutputEnv(t, envIntentUID)
	// Intent B has run: its gateway-owned output Secret exists.
	sB := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "b-outputs", Namespace: envNS,
		Labels:          map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, labelIntentUID: "b-uid"},
		OwnerReferences: []metav1.OwnerReference{ownerRef("mr-b-uid", true)}}, Data: map[string][]byte{"k": []byte("b-secret")}}
	if _, err := env.kube.CoreV1().Secrets(envNS).Create(context.Background(), sB, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// MR-B, re-parented to Intent A (the one with the open phase).
	mrB := managedResource(envIntentUID)
	mrB.Name, mrB.UID, mrB.Spec.OutputsSecretRef = "mr-b", "mr-b-uid", &lastv1alpha1.LocalRef{Name: "b-outputs"}
	if err := env.crd.Create(context.Background(), mrB); err != nil {
		t.Fatal(err)
	}
	_, err := env.writeOutputs(h, "mr-b", map[string]string{passwordKey: passwordRef})
	env.wantDenied(err, codes.PermissionDenied)
	if got := env.secret("b-outputs"); string(got.Data["k"]) != "b-secret" || got.Data[passwordKey] != nil {
		t.Fatalf("Intent B's secret was modified: %v", got.Data)
	}
}

func TestWriteOutputSecretForeignSecretVariants(t *testing.T) {
	mine := func(uid types.UID, controller bool) []metav1.OwnerReference {
		return []metav1.OwnerReference{ownerRef(uid, controller)}
	}
	managed := map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue}
	cases := map[string]*corev1.Secret{
		"managed-by but another MR":                     {ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, labelIntentUID: envIntentUID}, OwnerReferences: mine("other-mr", true)}},
		"this MR but unlabelled":                        {ObjectMeta: metav1.ObjectMeta{OwnerReferences: mine("mr-uid", true)}},
		"this MR but another intent uid":                {ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, labelIntentUID: "b-uid"}, OwnerReferences: mine("mr-uid", true)}},
		"this MR but not the controller":                {ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, labelIntentUID: envIntentUID}, OwnerReferences: mine("mr-uid", false)}},
		"controller and intent label but no managed-by": {ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelIntentUID: envIntentUID}, OwnerReferences: mine("mr-uid", true)}},
		"managed-by without intent label":               {ObjectMeta: metav1.ObjectMeta{Labels: managed, OwnerReferences: mine("mr-uid", true)}},
	}
	for name, sec := range cases {
		t.Run(name, func(t *testing.T) {
			env, h := newOutputEnv(t, envIntentUID)
			sec.Name, sec.Namespace, sec.Data = outputSecret, envNS, map[string][]byte{"k": []byte("keep")}
			if _, err := env.kube.CoreV1().Secrets(envNS).Create(context.Background(), sec, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err := env.writeOutputs(h, outputMR, map[string]string{passwordKey: passwordRef})
			env.wantDenied(err, codes.PermissionDenied)
			if got := env.secret(outputSecret); string(got.Data["k"]) != "keep" || got.Data[passwordKey] != nil {
				t.Fatalf("data = %v", got.Data)
			}
		})
	}
}

func TestWriteOutputSecretUpdateKeepsAnnotationsAndReplacesData(t *testing.T) {
	env, h := newOutputEnv(t, envIntentUID)
	if _, err := env.writeOutputs(h, outputMR, map[string]string{passwordKey: passwordRef}); err != nil {
		t.Fatal(err)
	}
	s := env.secret(outputSecret)
	s.Data["extra"] = []byte("stale")
	s.Annotations = map[string]string{"note": "keep-me"}
	s.Labels["team"] = "x"
	if _, err := env.kube.CoreV1().Secrets(envNS).Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.writeOutputs(h, outputMR, map[string]string{passwordKey: passwordRef}); err != nil {
		t.Fatal(err)
	}
	got := env.secret(outputSecret)
	if got.Annotations["note"] != "keep-me" || got.Labels["team"] != "x" || got.Labels[labelIntentUID] != envIntentUID || len(got.Data) != 1 {
		t.Fatalf("secret = %+v", got)
	}
	if c := got.OwnerReferences[0].Controller; c == nil || !*c {
		t.Fatal("the ManagedResource ownerReference is not a controller reference")
	}
}

func TestWriteOutputSecretConcurrentChangeIsAborted(t *testing.T) {
	for _, verb := range []string{"create", "update"} {
		t.Run(verb, func(t *testing.T) {
			env, h := newOutputEnv(t, envIntentUID)
			values := map[string]string{passwordKey: passwordRef}
			gr := schema.GroupResource{Resource: "secrets"}
			if verb == "update" {
				if _, err := env.writeOutputs(h, outputMR, values); err != nil {
					t.Fatal(err)
				}
			}
			var auditedFirst bool
			n := len(env.entries())
			env.kube.PrependReactor(verb, "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				auditedFirst = len(env.entries()) == n+1 && env.last().Type == audit.TypeOutputWritten
				if verb == "create" {
					return true, nil, apierrors.NewAlreadyExists(gr, outputSecret)
				}
				return true, nil, apierrors.NewConflict(gr, outputSecret, errors.New("stale"))
			})
			_, err := env.writeOutputs(h, outputMR, values)
			if status.Code(err) != codes.Aborted || !strings.Contains(err.Error(), "retry") {
				t.Fatalf("err = %v, want Aborted", err)
			}
			if !auditedFirst || env.last().Type != audit.TypeOutputWritten {
				t.Fatal("output.written was not durable before the write")
			}
		})
	}
}
