/*
QuestionReconciler is the hands of the gate. Sidecars only ever WRITE
Questions; this controller — the one identity with real permissions — carries
out an allowed action exactly once and reports the result back into the
Question, where the blocked tool call picks it up.
*/
package controller

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1 "github.com/tiny-systems/controller/api/v1alpha1"
)

// QuestionReconciler executes allowed Question actions.
type QuestionReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=agents.tinysystems.io,resources=questions,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.tinysystems.io,resources=questions/status,verbs=get;update;patch

// Reconcile carries out the action behind an answered Question.
func (r *QuestionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	q := &agentsv1.Question{}
	if err := r.Get(ctx, req.NamespacedName, q); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if q.Spec.Action == nil || !q.Answered() || q.Status.ActionDone {
		return ctrl.Result{}, nil
	}

	if !allowAnswer(q.Status.Answer) {
		// A refusal is terminal: record it so nobody retries the act.
		q.Status.ActionDone = true
		q.Status.Result = "denied"
		return ctrl.Result{}, r.Status().Update(ctx, q)
	}

	result, err := r.execute(ctx, q)
	if err != nil {
		// The act failed; the agent should hear the failure, not wait
		// forever. Terminal on purpose — retrying mutations without a fresh
		// human decision is how surprises happen.
		result = "action failed: " + err.Error()
	}
	q.Status.ActionDone = true
	q.Status.Result = result
	return ctrl.Result{}, r.Status().Update(ctx, q)
}

func allowAnswer(a string) bool { return a == "allow" || a == "yes" }

func (r *QuestionReconciler) execute(ctx context.Context, q *agentsv1.Question) (string, error) {
	p := q.Spec.Action.Params
	switch q.Spec.Action.Type {
	case agentsv1.ActionExposePort:
		return r.exposePort(ctx, q.Namespace, p["pod"], p["name"], p["port"])
	case agentsv1.ActionCreateSession:
		return r.createSession(ctx, q, p["name"], p["task"], p["repo"])
	default:
		return "", fmt.Errorf("unknown action type %q", q.Spec.Action.Type)
	}
}

func (r *QuestionReconciler) exposePort(ctx context.Context, namespace, pod, name, portStr string) (string, error) {
	port, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("port %q is not a port", portStr)
	}
	if pod == "" {
		return "", fmt.Errorf("no pod to expose")
	}
	svcName := fmt.Sprintf("tiny-%s-%d", name, port)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "tiny-controller"},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"tinysystems.io/pod": pod},
			Ports:    []corev1.ServicePort{{Port: int32(port), TargetPort: intstr.FromInt32(int32(port))}},
		},
	}
	if err := r.labelPod(ctx, namespace, pod); err != nil {
		return "", err
	}
	if err := r.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create service: %w", err)
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", svcName, namespace, port), nil
}

func (r *QuestionReconciler) labelPod(ctx context.Context, namespace, name string) error {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
		return fmt.Errorf("find pod to expose: %w", err)
	}
	if pod.Labels["tinysystems.io/pod"] == name {
		return nil
	}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["tinysystems.io/pod"] = name
	return r.Update(ctx, pod)
}

func (r *QuestionReconciler) createSession(ctx context.Context, q *agentsv1.Question, name, task, repo string) (string, error) {
	if task == "" {
		return "", fmt.Errorf("no task for the new session")
	}
	s := &agentsv1.Session{
		ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace},
		Spec:       agentsv1.SessionSpec{Task: task, Repo: repo},
	}
	if name != "" {
		s.Name = name
	} else {
		s.GenerateName = "tiny-"
	}
	// The asker's session, when known, becomes the child's parent label so a
	// screen can render the tree — and a future depth/budget guard can walk it.
	if q.Spec.Session.Name != "" {
		s.Labels = map[string]string{"tinysystems.io/parent": q.Spec.Session.Name}
	}
	if err := r.Create(ctx, s); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return s.Name, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *QuestionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1.Question{}).
		Named("question").
		Complete(r)
}
