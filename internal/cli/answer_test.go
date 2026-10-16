package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func clarifying(answers ...lastv1alpha1.Answer) *lastv1alpha1.Intent {
	return testIntent(webName, func(in *lastv1alpha1.Intent) {
		in.Spec.Answers = answers
		in.Status.Phase = lastv1alpha1.PhaseClarifying
		in.Status.Questions = []lastv1alpha1.Question{
			{ID: "q1", Text: questionTier, Options: []string{tierInternal, tierPublic}},
			{ID: "q2", Text: questionPort},
		}
	})
}

func answersOf(t *testing.T, c client.Client) []lastv1alpha1.Answer {
	t.Helper()
	var in lastv1alpha1.Intent
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: webName}, &in); err != nil {
		t.Fatal(err)
	}
	return in.Spec.Answers
}

func TestAnswerRecordsEveryAnswerAndKeepsEarlierOnes(t *testing.T) {
	c := fakeClient(clarifying(lastv1alpha1.Answer{ID: "q0", Value: "an earlier round"}))
	d, out := depsFor(c, "1\nport 80\n")
	if err := execute(d, "answer", webName); err != nil {
		t.Fatal(err)
	}
	want := []lastv1alpha1.Answer{{ID: "q0", Value: "an earlier round"}, {ID: "q1", Value: tierInternal}, {ID: "q2", Value: port80}}
	if got := answersOf(t, c); !reflect.DeepEqual(got, want) {
		t.Fatalf("spec.answers = %+v, want %+v", got, want)
	}
	if !strings.Contains(out.String(), "Recorded 2 answers.") {
		t.Errorf("output:\n%s", out)
	}
}

func TestAnswerAsksOnlyUnansweredQuestions(t *testing.T) {
	c := fakeClient(clarifying(lastv1alpha1.Answer{ID: "q1", Value: tierPublic}))
	d, out := depsFor(c, "port 80\n")
	if err := execute(d, "answer", webName); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), questionTier) {
		t.Errorf("asked an answered question:\n%s", out)
	}
	want := []lastv1alpha1.Answer{{ID: "q1", Value: tierPublic}, {ID: "q2", Value: port80}}
	if got := answersOf(t, c); !reflect.DeepEqual(got, want) {
		t.Fatalf("spec.answers = %+v, want %+v", got, want)
	}
}

func TestAnswerWhenEveryQuestionHasAnAnswer(t *testing.T) {
	c := fakeClient(clarifying(lastv1alpha1.Answer{ID: "q1", Value: tierPublic}, lastv1alpha1.Answer{ID: "q2", Value: "80"}))
	d, out := depsFor(c, "")
	if err := execute(d, "answer", webName); err != nil || !strings.Contains(out.String(), "Every question already has an answer") {
		t.Fatalf("answer = %v:\n%s", err, out)
	}
}

func TestAnswerNeedsOpenQuestions(t *testing.T) {
	planning := testIntent(webName, func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhasePlanning })
	d, _ := depsFor(fakeClient(planning), "")
	if err := execute(d, "answer", webName); err == nil || !strings.Contains(err.Error(), "has no open questions (phase Planning)") {
		t.Fatalf("answer while planning = %v", err)
	}
}

func TestMergeAnswersReplacesByID(t *testing.T) {
	got := mergeAnswers([]lastv1alpha1.Answer{{ID: "a", Value: "1"}, {ID: "b", Value: "2"}},
		[]lastv1alpha1.Answer{{ID: "b", Value: "3"}, {ID: "c", Value: "4"}})
	want := []lastv1alpha1.Answer{{ID: "a", Value: "1"}, {ID: "b", Value: "3"}, {ID: "c", Value: "4"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeAnswers = %+v", got)
	}
}

const (
	webName      = "web"
	tierInternal = "internal"
	tierPublic   = "public"
	port80       = "port 80"
	nginxName    = "nginx"
	trueCmd      = "true"
	questionTier = "Which tier?"
	questionPort = "Which port?"
)
