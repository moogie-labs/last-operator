package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	outputValue  = "s3cr3t-value-123"
	outputSecret = "web-db-outputs"
	passwordKey  = "password"
	passwordRef  = "${steps.s1.json.password}"
)

func managedResource(ownerUID types.UID) *lastv1alpha1.ManagedResource {
	return &lastv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "web-db", Namespace: envNS, UID: "mr-uid",
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
	env, in := newRunEnv(t, twoSteps())
	if err := env.crd.Create(context.Background(), managedResource(ownerUID)); err != nil {
		t.Fatal(err)
	}
	if _, ok := runAll(t, env, in)["s1"]; !ok {
		t.Fatal("runAll returned no result for s1")
	}
	return env, in.Status.Plan.Hash
}

func TestWriteOutputSecretWritesHashedAuditAndOwnedSecret(t *testing.T) {
	env, h := newOutputEnv(t, "intent-uid")
	resp, err := env.writeOutputs(h, "web-db", map[string]string{passwordKey: passwordRef})
	if err != nil || resp.GetSecretName() != outputSecret {
		t.Fatalf("WriteOutputSecret = %v, %v", resp, err)
	}
	s, err := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), outputSecret, metav1.GetOptions{})
	if err != nil || string(s.Data[passwordKey]) != outputValue || s.Labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue ||
		len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "mr-uid" || s.OwnerReferences[0].Kind != "ManagedResource" {
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
	if p.Values[passwordKey] != "sha256:"+hex.EncodeToString(sum[:]) || p.Secret != outputSecret || p.ManagedResource != "web-db" {
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
		_, err := env.writeOutputs(h, "web-db", map[string]string{passwordKey: passwordRef})
		env.wantDenied(err, codes.PermissionDenied)
	})
	t.Run("existing user secret is never overwritten", func(t *testing.T) {
		env, h := newOutputEnv(t, "intent-uid")
		mine := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: outputSecret, Namespace: envNS}, Data: map[string][]byte{"k": []byte("user")}}
		if _, err := env.kube.CoreV1().Secrets(envNS).Create(context.Background(), mine, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		_, err := env.writeOutputs(h, "web-db", map[string]string{passwordKey: passwordRef})
		env.wantDenied(err, codes.PermissionDenied)
		s, _ := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), outputSecret, metav1.GetOptions{})
		if string(s.Data["k"]) != "user" || s.Data[passwordKey] != nil {
			t.Fatalf("the user's secret was modified: %+v", s.Data)
		}
	})
	t.Run("own secret is updated", func(t *testing.T) {
		env, h := newOutputEnv(t, "intent-uid")
		values := map[string]string{passwordKey: passwordRef}
		if _, err := env.writeOutputs(h, "web-db", values); err != nil {
			t.Fatal(err)
		}
		values["name"] = "${steps.s1.json.metadata.name}"
		if _, err := env.writeOutputs(h, "web-db", values); err != nil {
			t.Fatalf("second write = %v", err)
		}
		s, _ := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), outputSecret, metav1.GetOptions{})
		if string(s.Data["name"]) != envNS {
			t.Fatalf("data = %v", s.Data)
		}
	})
	t.Run("bad reference, bad key, literal value, no phase", func(t *testing.T) {
		env, h := newOutputEnv(t, "intent-uid")
		cases := map[string]struct {
			values map[string]string
			code   codes.Code
		}{
			"missing key": {map[string]string{"p": "${steps.s1.json.nope}"}, codes.FailedPrecondition},
			"literal":     {map[string]string{"p": "hunter2"}, codes.FailedPrecondition},
			"bad key":     {map[string]string{"pass word": passwordRef}, codes.InvalidArgument},
			"no values":   {map[string]string{}, codes.InvalidArgument},
		}
		for name, tc := range cases {
			_, err := env.writeOutputs(h, "web-db", tc.values)
			if status.Code(err) != tc.code || env.last().Type != audit.TypePolicyDenied {
				t.Errorf("%s: %v (last %s), want %v with policy.denied", name, err, env.last().Type, tc.code)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(string(env.last().Payload), "hunter2") {
				t.Errorf("%s: the literal leaked into the error or audit entry", name)
			}
		}
		_, err := env.writeOutputs("sha256:"+strings.Repeat("0", 64), "web-db", map[string]string{"p": "${steps.s1.stdout}"})
		env.wantDenied(err, codes.FailedPrecondition)
	})
}

func TestWriteOutputSecretMalformedSubjectWritesNothing(t *testing.T) {
	env, _ := newOutputEnv(t, "intent-uid")
	before := len(env.entries())
	_, err := env.writeOutputs("not-a-hash", "web-db", map[string]string{"p": "${steps.s1.stdout}"})
	if status.Code(err) != codes.InvalidArgument || len(env.entries()) != before {
		t.Fatalf("err = %v, entries %d -> %d", err, before, len(env.entries()))
	}
}

func TestWriteOutputSecretClosedPhaseAndConcurrency(t *testing.T) {
	env, h := newOutputEnv(t, "intent-uid")
	values := map[string]string{passwordKey: passwordRef}
	p := env.g.phases.get(envKey)
	p.mu.Lock()
	_, err := env.writeOutputs(h, "web-db", values)
	p.mu.Unlock()
	if status.Code(err) != codes.Aborted {
		t.Fatalf("concurrent write = %v, want Aborted", err)
	}
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	_, err = env.writeOutputs(h, "web-db", values)
	env.wantDenied(err, codes.FailedPrecondition)
	if _, gerr := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), outputSecret, metav1.GetOptions{}); gerr == nil {
		t.Fatal("a closed phase wrote the secret")
	}
}

func TestWriteOutputSecretMissingObjectsAndRefs(t *testing.T) {
	env, h := newOutputEnv(t, "intent-uid")
	values := map[string]string{passwordKey: passwordRef}
	before := len(env.entries())
	if _, err := env.writeOutputs(h, "nope", values); status.Code(err) != codes.NotFound || len(env.entries()) != before {
		t.Fatalf("missing managed resource = %v", err)
	}
	mr := managedResource("intent-uid")
	mr.Name, mr.UID, mr.Spec.OutputsSecretRef = "bare", "bare-uid", nil
	if err := env.crd.Create(context.Background(), mr); err != nil {
		t.Fatal(err)
	}
	_, err := env.writeOutputs(h, "bare", values)
	env.wantDenied(err, codes.InvalidArgument)
}

func TestWriteOutputSecretAuditPrecedesSecret(t *testing.T) {
	env, h := newOutputEnv(t, "intent-uid")
	var auditedAtCreate bool
	env.kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret); s.Name == outputSecret {
			auditedAtCreate = env.last().Type == audit.TypeOutputWritten
		}
		return false, nil, nil
	})
	if _, err := env.writeOutputs(h, "web-db", map[string]string{passwordKey: passwordRef}); err != nil {
		t.Fatal(err)
	}
	if !auditedAtCreate {
		t.Fatal("output.written was not durable before the Secret was created")
	}
}
