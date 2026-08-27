package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1 "github.com/tiny-systems/controller/api/v1alpha1"
)

func executorClient(t *testing.T, objs ...runtime.Object) *QuestionReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &QuestionReconciler{Client: fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&agentsv1.Question{}, &agentsv1.Session{}).
		Build()}
}

func reconcileQ(t *testing.T, r *QuestionReconciler, name string) *agentsv1.Question {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	q := &agentsv1.Question{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, q); err != nil {
		t.Fatal(err)
	}
	return q
}

// An allowed createSession becomes a real Session, parent-labelled, and the
// result names it — that's what the blocked tool call hands the agent.
func TestAllowedCreateSessionActs(t *testing.T) {
	q := &agentsv1.Question{
		ObjectMeta: metav1.ObjectMeta{Name: "q1", Namespace: "default"},
		Spec: agentsv1.QuestionSpec{
			Text:    "spawn?",
			Session: agentsv1.SessionRef{Name: "boss"},
			Action: &agentsv1.QuestionAction{
				Type:   agentsv1.ActionCreateSession,
				Params: map[string]string{"name": "worker-1", "task": "run the load test"},
			},
		},
		Status: agentsv1.QuestionStatus{Answer: "allow"},
	}
	r := executorClient(t, q)
	got := reconcileQ(t, r, "q1")
	if !got.Status.ActionDone || got.Status.Result != "worker-1" {
		t.Fatalf("status: %+v", got.Status)
	}
	s := &agentsv1.Session{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "worker-1", Namespace: "default"}, s); err != nil {
		t.Fatalf("session not created: %v", err)
	}
	if s.Spec.Task != "run the load test" || s.Labels["tinysystems.io/parent"] != "boss" {
		t.Fatalf("session: %+v labels=%v", s.Spec, s.Labels)
	}
	// Exactly once: a second reconcile changes nothing.
	before := got.Status
	if after := reconcileQ(t, r, "q1"); after.Status != before {
		t.Fatalf("re-executed: %+v", after.Status)
	}
}

// A deny is terminal and acts on nothing.
func TestDeniedActionActsOnNothing(t *testing.T) {
	q := &agentsv1.Question{
		ObjectMeta: metav1.ObjectMeta{Name: "q2", Namespace: "default"},
		Spec: agentsv1.QuestionSpec{
			Text:   "spawn?",
			Action: &agentsv1.QuestionAction{Type: agentsv1.ActionCreateSession, Params: map[string]string{"task": "x"}},
		},
		Status: agentsv1.QuestionStatus{Answer: "deny"},
	}
	r := executorClient(t, q)
	got := reconcileQ(t, r, "q2")
	if !got.Status.ActionDone || got.Status.Result != "denied" {
		t.Fatalf("status: %+v", got.Status)
	}
	list := &agentsv1.SessionList{}
	if err := r.List(context.Background(), list); err != nil || len(list.Items) != 0 {
		t.Fatalf("a denied action created something: %d", len(list.Items))
	}
}

// An allowed exposePort labels the pod and creates the Service; the result is
// the in-cluster URL.
func TestAllowedExposePortActs(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "boss-agent", Namespace: "default"}}
	q := &agentsv1.Question{
		ObjectMeta: metav1.ObjectMeta{Name: "q3", Namespace: "default"},
		Spec: agentsv1.QuestionSpec{
			Text: "expose?",
			Action: &agentsv1.QuestionAction{
				Type:   agentsv1.ActionExposePort,
				Params: map[string]string{"port": "3000", "pod": "boss-agent", "name": "boss"},
			},
		},
		Status: agentsv1.QuestionStatus{Answer: "allow"},
	}
	r := executorClient(t, q, pod)
	got := reconcileQ(t, r, "q3")
	if !got.Status.ActionDone || !strings.Contains(got.Status.Result, "http://tiny-boss-3000.default.svc:3000") {
		t.Fatalf("status: %+v", got.Status)
	}
	svc := &corev1.Service{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "tiny-boss-3000", Namespace: "default"}, svc); err != nil {
		t.Fatalf("service: %v", err)
	}
	if svc.Spec.Selector["tinysystems.io/pod"] != "boss-agent" {
		t.Fatalf("selector: %v", svc.Spec.Selector)
	}
	p := &corev1.Pod{}
	_ = r.Get(context.Background(), types.NamespacedName{Name: "boss-agent", Namespace: "default"}, p)
	if p.Labels["tinysystems.io/pod"] != "boss-agent" {
		t.Fatalf("pod label: %v", p.Labels)
	}
}

// A failed action reports the failure instead of leaving the agent waiting.
func TestFailedActionReportsInsteadOfHanging(t *testing.T) {
	q := &agentsv1.Question{
		ObjectMeta: metav1.ObjectMeta{Name: "q4", Namespace: "default"},
		Spec: agentsv1.QuestionSpec{
			Text:   "expose?",
			Action: &agentsv1.QuestionAction{Type: agentsv1.ActionExposePort, Params: map[string]string{"port": "3000", "pod": "gone", "name": "x"}},
		},
		Status: agentsv1.QuestionStatus{Answer: "allow"},
	}
	r := executorClient(t, q)
	got := reconcileQ(t, r, "q4")
	if !got.Status.ActionDone || !strings.Contains(got.Status.Result, "action failed") {
		t.Fatalf("status: %+v", got.Status)
	}
}
