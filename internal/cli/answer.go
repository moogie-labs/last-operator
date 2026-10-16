package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func hasAnswer(in *lastv1alpha1.Intent, id string) bool {
	return slices.ContainsFunc(in.Spec.Answers, func(a lastv1alpha1.Answer) bool { return a.ID == id })
}

// mergeAnswers returns old with each answer in add replacing the one with its
// id, or appended.
func mergeAnswers(old, add []lastv1alpha1.Answer) []lastv1alpha1.Answer {
	out := slices.Clone(old)
	for _, a := range add {
		if i := slices.IndexFunc(out, func(o lastv1alpha1.Answer) bool { return o.ID == a.ID }); i >= 0 {
			out[i] = a
			continue
		}
		out = append(out, a)
	}
	return out
}

// answerQuestions asks every open question that has no answer yet, then
// records the answers in spec.answers. Earlier answers stay: the operator
// refuses a new question that reuses an answered id, and relies on that record.
func answerQuestions(ctx context.Context, k *Kube, p *prompter, in *lastv1alpha1.Intent) error {
	var add []lastv1alpha1.Answer
	for _, q := range in.Status.Questions {
		if hasAnswer(in, q.ID) {
			continue
		}
		v, err := p.askQuestion(ctx, q)
		if err != nil {
			return err
		}
		add = append(add, lastv1alpha1.Answer{ID: q.ID, Value: v})
	}
	if len(add) == 0 {
		writeln(p.w, "Every question already has an answer; the operator continues planning.")
		return nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur lastv1alpha1.Intent
		if err := k.Client.Get(ctx, client.ObjectKeyFromObject(in), &cur); err != nil {
			return err
		}
		cur.Spec.Answers = mergeAnswers(cur.Spec.Answers, add)
		return k.Client.Update(ctx, &cur)
	})
	if err != nil {
		return fmt.Errorf("record the answers: %w", err)
	}
	writef(p.w, "Recorded %d answers.\n", len(add))
	return nil
}

func newAnswerCommand(d Deps, kf *KubeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "answer NAME",
		Short: "Answer an Intent's open clarifying questions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			in, err := getIntent(cmd.Context(), k, args[0])
			if err != nil {
				return err
			}
			if in.Status.Phase != lastv1alpha1.PhaseClarifying || len(in.Status.Questions) == 0 {
				return fmt.Errorf("intent %s/%s has no open questions (phase %s)", in.Namespace, in.Name, phaseOf(in))
			}
			err = answerQuestions(cmd.Context(), k, newPrompter(d.In, d.Out), in)
			switch {
			case err != nil && cmd.Context().Err() != nil:
				writef(d.Out, "Stopped answering Intent %s/%s. See where it is with kubectl last plan %s -n %s.\n",
					in.Namespace, in.Name, in.Name, in.Namespace)
				return nil
			case errors.Is(err, errNoInput):
				return fmt.Errorf("%w while Intent %s/%s waited for your answers; answer them later with kubectl last answer %s -n %s",
					err, in.Namespace, in.Name, in.Name, in.Namespace)
			}
			return err
		},
	}
}
