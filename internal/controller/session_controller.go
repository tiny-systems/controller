/*
SessionReconciler turns a Session into a workload: one pod holding the agent
and its MCP sidecar, and a persistent workspace that outlives the pod. The
workspace is the durability — a rescheduled pod finds the transcript and the
checkout where the last one left them, and the agent resumes.
*/
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1 "github.com/tiny-systems/controller/api/v1alpha1"
	"github.com/tiny-systems/controller/internal/tools"
)

// Images the controller wires in when the Session doesn't say otherwise.
// Overridable per deployment; pinned defaults keep a bare Session runnable.
type Images struct {
	// Agent is the default coding-agent image.
	Agent string
	// Sidecar is the MCP toolbox image (this repo's own image).
	Sidecar string
}

// SessionReconciler reconciles a Session object.
type SessionReconciler struct {
	client.Client
	Images Images
}

// +kubebuilder:rbac:groups=agents.tinysystems.io,resources=sessions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.tinysystems.io,resources=sessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.tinysystems.io,resources=sessions/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods;persistentvolumeclaims;services,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives one Session toward its workload.
func (r *SessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	session := &agentsv1.Session{}
	if err := r.Get(ctx, req.NamespacedName, session); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.ensureWorkspace(ctx, session); err != nil {
		return ctrl.Result{}, err
	}
	pod, err := r.ensurePod(ctx, session)
	if err != nil {
		return ctrl.Result{}, err
	}

	phase, msg := phaseOf(pod)
	if session.Status.Phase != phase || session.Status.Pod != pod.Name || session.Status.Message != msg {
		session.Status.Phase = phase
		session.Status.Pod = pod.Name
		session.Status.Message = msg
		if err := r.Status().Update(ctx, session); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func workspaceName(s *agentsv1.Session) string { return s.Name + "-workspace" }
func podName(s *agentsv1.Session) string       { return s.Name + "-agent" }

func (r *SessionReconciler) ensureWorkspace(ctx context.Context, s *agentsv1.Session) error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: workspaceName(s)}, pvc)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	size := s.Spec.WorkspaceSize
	if size == "" {
		size = "2Gi"
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("workspaceSize %q: %w", size, err)
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceName(s), Namespace: s.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	if err := controllerutil.SetControllerReference(s, pvc, r.Scheme()); err != nil {
		return err
	}
	return r.Create(ctx, pvc)
}

func (r *SessionReconciler) ensurePod(ctx context.Context, s *agentsv1.Session) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: podName(s)}, pod)
	if err == nil {
		return pod, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	agentImage := s.Spec.Image
	if agentImage == "" {
		agentImage = r.Images.Agent
	}

	workspace := corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"}
	pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName(s),
			Namespace: s.Namespace,
			Labels: map[string]string{
				tools.SessionLabel: s.Name,
				"app":              "tiny-session",
			},
		},
		Spec: corev1.PodSpec{
			// The workspace survives; the pod is disposable. Never restart in
			// place — let the controller reconcile a fresh pod that resumes.
			RestartPolicy: corev1.RestartPolicyNever,
			// The session's identity: may create Questions, nothing else.
			ServiceAccountName: "tiny-session",
			// The agent runs unprivileged (uid 61000); fsGroup makes the
			// freshly-provisioned workspace volume writable for it.
			SecurityContext: &corev1.PodSecurityContext{
				FSGroup: ptr(int64(61000)),
			},
			// hostPath-backed provisioners (minikube, kind) ignore fsGroup, so
			// ownership is set explicitly, once, by a root init container.
			// fsGroup above still covers CSI volumes that do it properly.
			InitContainers: []corev1.Container{{
				Name:    "workspace-perms",
				Image:   "busybox:1.36",
				Command: []string{"sh", "-c", "chown 61000:61000 /workspace"},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: ptr(int64(0)),
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
			}},
			Volumes: []corev1.Volume{{
				Name: "workspace",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: workspaceName(s)},
				},
			}},
			Containers: []corev1.Container{
				{
					Name:       "agent",
					Image:      agentImage,
					WorkingDir: "/workspace",
					// Credentials by convention: a Secret named tiny-agent-env
					// in the namespace (ANTHROPIC_API_KEY and friends) lands in
					// the agent's environment. Optional — a cluster without it
					// still schedules; the agent then reports the missing key
					// as a question instead of crashlooping.
					EnvFrom: []corev1.EnvFromSource{{
						SecretRef: &corev1.SecretEnvSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "tiny-agent-env"},
							Optional:             ptr(true),
						},
					}},
					Env: []corev1.EnvVar{
						{Name: "TINY_TASK", Value: s.Spec.Task},
						{Name: "TINY_REPO", Value: s.Spec.Repo},
						{Name: "TINY_SESSION_NAME", Value: s.Name},
					},
					VolumeMounts: []corev1.VolumeMount{workspace},
				},
				{
					Name:  "tiny-mcp",
					Image: r.Images.Sidecar,
					Args:  []string{"serve", "--addr=127.0.0.1:8080"},
					Env: []corev1.EnvVar{
						{Name: "TINY_SESSION_NAME", Value: s.Name},
						{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
						}},
						{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
						}},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(s, pod, r.Scheme()); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

// phaseOf maps a pod to the session's coarse state.
func phaseOf(pod *corev1.Pod) (agentsv1.SessionPhase, string) {
	switch pod.Status.Phase {
	case corev1.PodRunning:
		return agentsv1.SessionRunning, ""
	case corev1.PodSucceeded:
		return agentsv1.SessionDone, ""
	case corev1.PodFailed:
		return agentsv1.SessionFailed, pod.Status.Message
	default:
		msg := ""
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				msg = c.Message
			}
		}
		return agentsv1.SessionPending, msg
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1.Session{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("session").
		Complete(r)
}

// ptr gives a pointer to any value — the small helper the core API's
// Optional fields keep asking for.
func ptr[T any](v T) *T { return &v }
