package cli

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func TestIntentNameSlugsTheFirstWords(t *testing.T) {
	for request, want := range map[string]string{
		"Run an nginx web server serving a hello page": "run-an-nginx-web-server-abcd",
		"  Deploy   Redis!! v7 ":                       "deploy-redis-v7-abcd",
		"!!! ???":                                      "intent-abcd",
		strings.Repeat("a", 50) + " x":                 strings.Repeat("a", 30) + "-abcd",
	} {
		if got := intentName(request, "abcd"); got != want {
			t.Errorf("intentName(%q) = %q, want %q", request, got, want)
		}
	}
}

func intents(t *testing.T, c client.Client) []lastv1alpha1.Intent {
	t.Helper()
	var list lastv1alpha1.IntentList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestCreateWithoutFollowing(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "")
	if err := execute(d, "run nginx", "--no-follow"); err != nil {
		t.Fatal(err)
	}
	got := intents(t, c)
	if len(got) != 1 || got[0].Name != "run-nginx-abcd" || got[0].Spec.Request != "run nginx" {
		t.Fatalf("intents = %+v", got)
	}
	if out.String() != "Created Intent team-x/run-nginx-abcd\n" {
		t.Fatalf("output = %q", out)
	}
}

func TestCreateRefusesAOneWordRequest(t *testing.T) {
	c := fakeClient()
	d, _ := depsFor(c, "")
	for _, args := range [][]string{{"aprove"}, {"aprove", webName}, {"run", "nginx", "now"}, {"  "}} {
		err := execute(d, args...)
		if err == nil {
			t.Errorf("kubectl last %q succeeded", args)
			continue
		}
		if len(strings.TrimSpace(args[0])) > 0 && (!strings.Contains(err.Error(), "unknown command "+`"`+args[0]+`"`) ||
			!strings.Contains(err.Error(), "quote a request in your own words")) {
			t.Errorf("kubectl last %q = %v", args, err)
		}
	}
	if n := len(intents(t, c)); n != 0 {
		t.Fatalf("%d Intents created", n)
	}
}
