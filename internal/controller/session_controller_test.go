/*
The contract under test: a Session becomes a pod with the agent and the
localhost MCP sidecar, plus a workspace PVC that the pod mounts — and the
status mirrors the pod. A fake client suffices: no kubelet runs pods under
envtest either, and the shape is the contract.
*/
package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1 "github.com/tiny-systems/controller/api/v1alpha1"
)

func TestSessionBecomesPodAndWorkspace(t *testing.T) {
	ctx := context.Background()
	r := &SessionReconciler{
		Client: sessionTestClient(t),
		Images: Images{Agent: "example.com/agent:test", Sidecar: "example.com/sidecar:test"},
	}

	s := &agentsv1.Session{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-flaky", Namespace: "default"},
		Spec:       agentsv1.SessionSpec{Task: "fix the flaky checkout test", Repo: "https://example.com/acme/shop.git"},
	}
	if err := r.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "fix-flaky", Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: "fix-flaky-workspace", Namespace: "default"}, pvc); err != nil {
		t.Fatalf("workspace PVC: %v", err)
	}

	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: "fix-flaky-agent", Namespace: "default"}, pod); err != nil {
		t.Fatalf("agent pod: %v", err)
	}
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("want agent + sidecar, got %d containers", len(pod.Spec.Containers))
	}
	agent, sidecar := pod.Spec.Containers[0], pod.Spec.Containers[1]
	if agent.Image != "example.com/agent:test" || sidecar.Image != "example.com/sidecar:test" {
		t.Fatalf("images: %s / %s", agent.Image, sidecar.Image)
	}
	if got := envOf(agent, "TINY_TASK"); got != "fix the flaky checkout test" {
		t.Fatalf("task env: %q", got)
	}
	if got := envOf(sidecar, "TINY_SESSION_NAME"); got != "fix-flaky" {
		t.Fatalf("sidecar identity env: %q", got)
	}
	if pod.Labels["tinysystems.io/session"] != "fix-flaky" {
		t.Fatalf("session label missing: %v", pod.Labels)
	}
	if len(pod.OwnerReferences) == 0 || pod.OwnerReferences[0].Name != "fix-flaky" {
		t.Fatal("pod must be owned by its Session — deletion must cascade")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("pods are disposable; the controller reconciles replacements")
	}

	// Status mirrors the pod (Pending — envtest schedules nothing).
	got := &agentsv1.Session{}
	if err := r.Get(ctx, types.NamespacedName{Name: "fix-flaky", Namespace: "default"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Pod != "fix-flaky-agent" || got.Status.Phase != agentsv1.SessionPending {
		t.Fatalf("status: %+v", got.Status)
	}

	// Reconcile again: no duplicates, no churn.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "fix-flaky", Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
}

func envOf(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func sessionTestClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1.Session{}).
		Build()
}
