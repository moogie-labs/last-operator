package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// ErrIntentFailed is returned when a followed Intent fails, after its reason is printed.
var ErrIntentFailed = errors.New("the Intent failed")

// answered reports whether spec.answers answers every open question, as the
// operator checks it before planning on.
func answered(in *lastv1alpha1.Intent) bool {
	for _, q := range in.Status.Questions {
		if !hasAnswer(in, q.ID) {
			return false
		}
	}
	return true
}

// follow reads the Intent every Wait and acts on what it waits for (M3 spec
// §5): it asks the open questions, and puts each discovery command and plan to
// the user once. It returns at Ready or Drifted, fails at Failed, and on Ctrl-C
// stops following without touching the Intent.
func follow(ctx context.Context, d Deps, k *Kube, key client.ObjectKey) error {
	p := newPrompter(d.In, d.Out)
	asked := map[string]bool{}  // subjects already put to the user, decided or left pending
	warned := map[string]bool{} // stated hashes whose content did not hash to them
	var last lastv1alpha1.Phase
	var prev *lastv1alpha1.Intent // the last Intent read
	for {
		var in lastv1alpha1.Intent
		if err := k.Client.Get(ctx, key, &in); err != nil {
			switch {
			case ctx.Err() != nil:
				return stopped(d.Out, key)
			case prev != nil && apierrors.IsNotFound(err):
				// Only an Intent that was being deleted and tearing down went through its teardown.
				// Any other disappearance (a force delete, a finalizer removed by hand) may have left resources behind.
				if prev.DeletionTimestamp != nil && prev.Status.Phase == lastv1alpha1.PhaseTearingDown {
					writef(d.Out, "Intent %s/%s torn down.\n", key.Namespace, key.Name)
				} else {
					writef(d.Out, "Intent %s/%s was deleted.\n", key.Namespace, key.Name)
				}
				return nil
			}
			return err
		}
		prev = in.DeepCopy()
		if phaseOf(&in) != last {
			printStatus(d.Out, &in)
			last = phaseOf(&in)
		}
		var err error
		switch in.Status.Phase {
		case lastv1alpha1.PhaseClarifying:
			if len(in.Status.Questions) > 0 && !answered(&in) {
				err = answerQuestions(ctx, k, p, &in)
			}
		case lastv1alpha1.PhaseAwaitingDiscoveryApproval, lastv1alpha1.PhaseAwaitingApproval:
			stage, subject, ok, verr := verifiedSubject(&in)
			switch {
			case !ok:
			case verr != nil:
				if !warned[subject] {
					warned[subject] = true
					writef(d.Out, "Warning: %s\n", verr)
				}
			case !asked[subject]:
				asked[subject] = true
				err = putToUser(ctx, k, p, &in, stage, subject)
			}
		case lastv1alpha1.PhaseReady:
			return printReady(ctx, k, d.Out, &in)
		case lastv1alpha1.PhaseDrifted:
			return nil // the status line says why; the operator probes the resources
		case lastv1alpha1.PhaseFailed:
			return failed(d.Out, &in)
		}
		switch {
		case ctx.Err() != nil:
			return stopped(d.Out, key)
		case errors.Is(err, errNoInput):
			return fmt.Errorf("%w while Intent %s/%s waited for you; see where it is with kubectl last plan %s -n %s, "+
				"then use kubectl last answer, approve or deny", err, key.Namespace, key.Name, key.Name, key.Namespace)
		case err != nil:
			return err
		}
		if err := d.Wait(ctx); err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return stopped(d.Out, key)
			}
			return err
		}
	}
}

// putToUser shows a verified discovery command or plan, with its files'
// contents if asked, and records the user's decision on it.
func putToUser(ctx context.Context, k *Kube, p *prompter, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject string) error {
	files := hasFiles(in, stage)
	writeln(p.w)
	renderSubject(p.w, in, stage, subject, false)
	for {
		decision, reason, err := p.askDecision(ctx, stage, files)
		switch {
		case err != nil:
			return err
		case decision == decisionShowFiles:
			writeln(p.w)
			renderSubject(p.w, in, stage, subject, true)
			continue
		case decision == "":
			writeln(p.w, "Left pending. Decide on it later:")
			printDecisionCommands(p.w, in, subject)
			return nil
		}
		return decide(ctx, k, p.w, in, stage, subject, decision, reason)
	}
}

// printReady lists what the Intent provisioned: its ManagedResources and their output Secrets.
func printReady(ctx context.Context, k *Kube, w io.Writer, in *lastv1alpha1.Intent) error {
	writef(w, "Intent %s/%s is Ready.\n", in.Namespace, in.Name)
	var list lastv1alpha1.ManagedResourceList
	if err := k.Client.List(ctx, &list, client.InNamespace(in.Namespace)); err != nil {
		return err
	}
	slices.SortFunc(list.Items, func(a, b lastv1alpha1.ManagedResource) int { return strings.Compare(a.Name, b.Name) })
	for _, mr := range list.Items {
		if mr.Spec.IntentRef == nil || mr.Spec.IntentRef.Name != in.Name {
			continue
		}
		writef(w, "  ManagedResource %s: %s\n", mr.Name, clean(mr.Spec.Description))
		if mr.Spec.OutputsSecretRef != nil {
			writef(w, "    outputs: Secret %s\n", clean(mr.Spec.OutputsSecretRef.Name))
		}
	}
	return nil
}

func failed(w io.Writer, in *lastv1alpha1.Intent) error {
	reason, msg := "", ""
	if c := meta.FindStatusCondition(in.Status.Conditions, condReady); c != nil {
		reason, msg = c.Reason, c.Message
	}
	again := "Edit its spec.request to try again."
	if in.Status.Stage == lastv1alpha1.StageDelete {
		again = "Change its " + lastv1alpha1.AnnotationRetry + " annotation to plan the teardown again."
	}
	writef(w, "Intent %s/%s failed: %s: %s\n%s\n", in.Namespace, in.Name, clean(reason), clean(msg), again)
	return ErrIntentFailed
}

func stopped(w io.Writer, key client.ObjectKey) error {
	writef(w, "\nStopped following Intent %s/%s; it carries on. See where it is with kubectl last plan %s -n %s.\n",
		key.Namespace, key.Name, key.Name, key.Namespace)
	return nil
}
