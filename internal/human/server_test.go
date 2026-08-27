package human

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	tinyv1 "github.com/tiny-systems/controller/api/v1alpha1"
)

func testServer(t *testing.T, objs ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := tinyv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &Server{
		Client:       fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Namespace:    "agents",
		PollInterval: 10 * time.Millisecond,
	}
}

// The whole contract in one test: ask parks a Question, a human writes the
// answer into status, the blocked call returns it.
func TestAskBlocksUntilAnswered(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()

	type result struct {
		out AskOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, out, err := s.ask(ctx, nil, AskInput{Question: "Force-push the rebased branch?", Options: []string{"yes", "no"}})
		done <- result{out, err}
	}()

	// The question appears.
	var q tinyv1.Question
	deadline := time.Now().Add(2 * time.Second)
	for {
		list := &tinyv1.QuestionList{}
		if err := s.Client.List(ctx, list, client.InNamespace("agents")); err == nil && len(list.Items) == 1 {
			q = list.Items[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("question never created")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if q.Spec.Text != "Force-push the rebased branch?" || q.Spec.Reason != ReasonTool {
		t.Fatalf("question %+v", q.Spec)
	}
	select {
	case r := <-done:
		t.Fatalf("ask returned before anyone answered: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	// The human answers.
	q.Status.Answer = "yes"
	now := metav1.Now()
	q.Status.AnsweredAt = &now
	if err := s.Client.Update(ctx, &q); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil || r.out.Answer != "yes" {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask never returned after the answer landed")
	}
}

// An interrupted wait must hand back the question id, so the model resumes
// with await_answer instead of putting a duplicate card in front of the human.
func TestInterruptNamesTheQuestion(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, _, err := s.ask(ctx, nil, AskInput{Question: "proceed?"})
		done <- err
	}()
	// Let the question be created, then cut the call.
	time.Sleep(50 * time.Millisecond)
	cancel()
	err := <-done
	if err == nil {
		t.Fatal("expected an interruption error")
	}
	list := &tinyv1.QuestionList{}
	if lerr := s.Client.List(context.Background(), list, client.InNamespace("agents")); lerr != nil || len(list.Items) != 1 {
		t.Fatalf("question should survive the interruption: %v %d", lerr, len(list.Items))
	}
	if want := list.Items[0].Name; !bytes.Contains([]byte(err.Error()), []byte(want)) {
		t.Fatalf("error %q does not name question %q", err, want)
	}

	// And await_answer picks it up.
	q := list.Items[0]
	q.Status.Answer = "go ahead"
	if uerr := s.Client.Update(context.Background(), &q); uerr != nil {
		t.Fatal(uerr)
	}
	_, out, aerr := s.await(context.Background(), nil, AwaitInput{QuestionID: q.Name})
	if aerr != nil || out.Answer != "go ahead" {
		t.Fatalf("await: %v %+v", aerr, out)
	}
}

// The safety net: hooks POST /attention. One open notification per session —
// hooks fire repeatedly, and a card per firing would bury the human.
func TestAttentionDedupesPerSession(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	post := func(body map[string]string) map[string]string {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/attention", bytes.NewReader(b))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("attention: %d %s", w.Code, w.Body.String())
		}
		var out map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}

	first := post(map[string]string{"message": "waiting at a permission prompt", "session": "flaky-test"})
	second := post(map[string]string{"message": "still waiting", "session": "flaky-test"})
	if first["questionId"] != second["questionId"] || second["deduped"] != "true" {
		t.Fatalf("expected dedupe onto one question: %v then %v", first, second)
	}

	list := &tinyv1.QuestionList{}
	if err := s.Client.List(context.Background(), list, client.InNamespace("agents")); err != nil || len(list.Items) != 1 {
		t.Fatalf("want exactly one open notification, got %d", len(list.Items))
	}
	if got := list.Items[0].Spec.Text; got != "still waiting" {
		t.Fatalf("text should refresh to the latest message, got %q", got)
	}

	// Once answered, a new firing opens a fresh card.
	q := list.Items[0]
	q.Status.Answer = "handled"
	if err := s.Client.Update(context.Background(), &q); err != nil {
		t.Fatal(err)
	}
	third := post(map[string]string{"message": "new question", "session": "flaky-test"})
	if third["questionId"] == first["questionId"] {
		t.Fatal("an answered notification must not be reused")
	}
}
