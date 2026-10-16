package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// maxSlug bounds the words of a generated Intent name, before its suffix.
const maxSlug = 30

type createFlags struct {
	name     string
	noFollow bool
}

// runCreate creates an Intent for request and, unless told not to, follows it.
func runCreate(ctx context.Context, d Deps, kf KubeFlags, cf createFlags, request string) error {
	request = strings.TrimSpace(request)
	if request == "" {
		return errors.New("the request is empty")
	}
	if !strings.ContainsFunc(request, unicode.IsSpace) {
		return fmt.Errorf("unknown command %q; to create an Intent, quote a request in your own words, "+
			`e.g. kubectl last "run an nginx web server"`, request)
	}
	k, err := d.Kube(kf)
	if err != nil {
		return err
	}
	name := cf.name
	if name == "" {
		name = intentName(request, d.Suffix())
	}
	in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.Namespace},
		Spec: lastv1alpha1.IntentSpec{Request: request}}
	if err := k.Client.Create(ctx, in); err != nil {
		return fmt.Errorf("create the Intent: %w", err)
	}
	writef(d.Out, "Created Intent %s/%s\n", in.Namespace, in.Name)
	if cf.noFollow {
		return nil
	}
	return follow(ctx, d, k, client.ObjectKeyFromObject(in))
}

// intentName is a DNS-safe slug of the request's first words, plus suffix.
func intentName(request, suffix string) string {
	words := strings.FieldsFunc(strings.ToLower(request), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	var b strings.Builder
	for _, w := range words {
		if b.Len() == 0 && len(w) > maxSlug {
			w = w[:maxSlug]
		}
		if b.Len() > 0 && b.Len()+1+len(w) > maxSlug {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('-')
		}
		b.WriteString(w)
	}
	if b.Len() == 0 {
		b.WriteString("intent")
	}
	return b.String() + "-" + suffix
}
