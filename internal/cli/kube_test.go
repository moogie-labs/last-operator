package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	testNS   = "team-x"
	testUser = "alice"
)

var (
	h1 = "sha256:" + strings.Repeat("1", 64)
	h2 = "sha256:" + strings.Repeat("2", 64)
)

// fakeClient is an API server holding objs. Intent status is a subresource, as
// in the real CRD, so a spec update never touches it.
func fakeClient(objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(newScheme()).
		WithStatusSubresource(&lastv1alpha1.Intent{}).WithObjects(objs...).Build()
}

// depsFor runs the CLI against c as testUser in testNS, with input as what the
// user types.
//
//nolint:unparam // later tasks feed input to prompts
func depsFor(c client.Client, input string) (Deps, *bytes.Buffer) {
	var out bytes.Buffer
	return Deps{
		Out: &out, Err: &out, In: strings.NewReader(input),
		Getenv: func(string) string { return "" },
		Kube: func(KubeFlags) (*Kube, error) {
			return &Kube{Client: c, Namespace: testNS,
				WhoAmI: func(context.Context) (string, error) { return testUser, nil }}, nil
		},
		Wait:   func(context.Context) error { return errors.New("the test did not expect to wait") },
		Suffix: func() string { return "abcd" },
	}, &out
}

func testIntent(name string, change func(*lastv1alpha1.Intent)) *lastv1alpha1.Intent {
	in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: lastv1alpha1.IntentSpec{Request: "run nginx"}}
	if change != nil {
		change(in)
	}
	return in
}

// setReady sets the Ready condition the operator writes on every phase change.
func setReady(in *lastv1alpha1.Intent, reason, msg string) {
	meta.SetStatusCondition(&in.Status.Conditions, metav1.Condition{Type: condReady, Status: metav1.ConditionFalse,
		Reason: reason, Message: msg})
}

func argvCommand(argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: argv}}
}
