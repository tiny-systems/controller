package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1 "github.com/tiny-systems/controller/api/v1alpha1"
)

// SessionInfo is one row of session_list.
type SessionInfo struct {
	Name    string `json:"name"`
	Phase   string `json:"phase,omitempty"`
	Task    string `json:"task,omitempty"`
	Pod     string `json:"pod,omitempty"`
	Message string `json:"message,omitempty"`
}

// ListInput has no parameters yet; reserved for filters.
type ListInput struct{}

// ListOutput carries the rows.
type ListOutput struct {
	Sessions []SessionInfo `json:"sessions"`
}

func (s *Server) sessionList(ctx context.Context, _ *mcp.CallToolRequest, _ ListInput) (*mcp.CallToolResult, ListOutput, error) {
	list := &agentsv1.SessionList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, ListOutput{}, fmt.Errorf("list sessions: %w", err)
	}
	out := ListOutput{Sessions: make([]SessionInfo, 0, len(list.Items))}
	for _, it := range list.Items {
		out.Sessions = append(out.Sessions, SessionInfo{
			Name:    it.Name,
			Phase:   string(it.Status.Phase),
			Task:    it.Spec.Task,
			Pod:     it.Status.Pod,
			Message: it.Status.Message,
		})
	}
	return nil, out, nil
}

// CreateInput is what an agent supplies to start a sibling session.
type CreateInput struct {
	Name string `json:"name,omitempty" jsonschema:"Name for the session. Generated when omitted."`
	Task string `json:"task" jsonschema:"The task the new session starts with."`
	Repo string `json:"repo,omitempty" jsonschema:"Repository to clone into the new session's workspace."`
}

// CreateOutput names what was made.
type CreateOutput struct {
	Name   string `json:"name"`
	Result string `json:"result,omitempty"`
}

// sessionCreate parks a createSession action behind the gate. The sidecar
// holds no permission to make sessions — it writes the request; the
// controller, once a human allows it, is the one that acts.
func (s *Server) sessionCreate(ctx context.Context, _ *mcp.CallToolRequest, in CreateInput) (*mcp.CallToolResult, CreateOutput, error) {
	if strings.TrimSpace(in.Task) == "" {
		return nil, CreateOutput{}, fmt.Errorf("task is required")
	}
	asker := s.sessionFor(ctx)
	result, err := s.requestAction(ctx, asker,
		fmt.Sprintf("Session %s wants to start a new session with the task: %q — allow?", orUnknown(asker.Name), in.Task),
		agentsv1.QuestionAction{
			Type: agentsv1.ActionCreateSession,
			Params: map[string]string{
				"name": in.Name,
				"task": in.Task,
				"repo": in.Repo,
			},
		})
	if err != nil {
		return nil, CreateOutput{}, err
	}
	return nil, CreateOutput{Name: result, Result: result}, nil
}

func orUnknown(v string) string {
	if v == "" {
		return "(unattributed)"
	}
	return v
}
