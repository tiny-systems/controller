/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SessionSpec is what a session runs: one agent, one task, one workspace.
type SessionSpec struct {
	// Task the agent starts with.
	// +kubebuilder:validation:MinLength=1
	Task string `json:"task"`
	// Repo to clone into the workspace before the agent starts. Optional —
	// a session can start on an empty workspace.
	// +optional
	Repo string `json:"repo,omitempty"`
	// Agent image to run. The image owns how the agent is started (tmux,
	// resume, hooks); the controller only wires task, workspace and sidecar.
	// +optional
	Image string `json:"image,omitempty"`
	// WorkspaceSize is the persistent workspace request. Defaults to 2Gi.
	// +optional
	WorkspaceSize string `json:"workspaceSize,omitempty"`
}

// SessionPhase is the coarse state a screen sorts by.
// +kubebuilder:validation:Enum=Pending;Running;Done;Failed
type SessionPhase string

// Session phases.
const (
	SessionPending SessionPhase = "Pending"
	SessionRunning SessionPhase = "Running"
	SessionDone    SessionPhase = "Done"
	SessionFailed  SessionPhase = "Failed"
)

// SessionStatus is what the controller observed.
type SessionStatus struct {
	// Phase mirrors the workload's coarse state.
	// +optional
	Phase SessionPhase `json:"phase,omitempty"`
	// Pod running the session, when one exists.
	// +optional
	Pod string `json:"pod,omitempty"`
	// Message says why, when a phase needs explaining.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Task",type=string,JSONPath=`.spec.task`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Session is one coding-agent session running as a workload: a pod holding
// the agent and its MCP sidecar, and a persistent workspace that outlives the
// pod so the session resumes where it stopped.
type Session struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SessionSpec   `json:"spec,omitempty"`
	Status SessionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SessionList contains a list of Session.
type SessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Session `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Session{}, &SessionList{})
		return nil
	})
}
