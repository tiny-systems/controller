/*
Package human is the ask-human surface: the gate an unattended agent reaches
for when it hits a decision it must not make alone.

Two doors into the same object:

  - The good path: the agent calls the ask_human MCP tool. A Question appears
    in the cluster and the call BLOCKS until a human writes an answer into its
    status — the answer is the tool result, and the agent continues.
  - The safety net: the agent ignored the tool and simply asked in its shell,
    or hit a permission prompt. A Claude Code Notification hook (or any
    runner) POSTs /attention, and the same Question appears — answered not by
    unblocking a tool call but by whoever attaches to the session.

Nothing here knows what a "session runner" is. kelos, a bare pod, anything
able to reach this server over the cluster network gets the same gate.
*/
package human

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tinyv1 "github.com/tiny-systems/controller/api/v1alpha1"
)

// Reasons a question exists. Tool questions block an agent mid-call; a
// notification question records that the agent is visibly waiting on a human
// through some other channel (its own prompt, a permission dialog).
const (
	ReasonTool         = "tool"
	ReasonNotification = "notification"
)

// SessionLabel lets screens select a session's questions without parsing spec.
const SessionLabel = "tinysystems.io/session"

// Server holds what the tools need: a cluster client and the namespace the
// Questions live in.
type Server struct {
	Client    client.Client
	Namespace string
	// PollInterval is how often a blocked ask checks for its answer. A watch
	// would be tidier; a poll on one named object is simpler and the latency
	// is human-scale anyway. Zero means 2s.
	PollInterval time.Duration
}

// AskInput is the ask_human argument shape shown to the model.
type AskInput struct {
	Question string   `json:"question" jsonschema:"What you need the human to decide, with enough context to answer without reading your transcript."`
	Options  []string `json:"options,omitempty" jsonschema:"Choices to offer as one-press buttons. Omit for a free-text answer."`
}

// AskOutput carries the human's decision back to the model.
type AskOutput struct {
	Answer string `json:"answer"`
}

// AwaitInput resumes waiting on a question an earlier call created.
type AwaitInput struct {
	QuestionID string `json:"questionId" jsonschema:"The id from the interrupted ask_human call's error message."`
}

// MCP builds the tool server.
func (s *Server) MCP() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "tiny-human", Version: "v0.1.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ask_human",
		Description: "Ask the human operator a question and WAIT for their answer. Use it before any action that is " +
			"hard to undo, when the task is ambiguous, or when you need information only they have. Give options when " +
			"the answer is a choice; omit them for free text. The call blocks until they answer — minutes or hours. " +
			"If it returns an interruption naming a questionId, call await_answer with that id instead of asking again.",
	}, s.ask)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "await_answer",
		Description: "Resume waiting for the answer to a question ask_human already created.",
	}, s.await)
	return srv
}

// Handler serves MCP on /mcp, the hook safety net on /attention, and liveness
// on /healthz — one port, one Service.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", withCallerIP(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.MCP() }, nil)))
	mux.HandleFunc("/attention", s.handleAttention)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	return mux
}

type ctxKey int

const callerIPKey ctxKey = 0

// withCallerIP stashes the HTTP peer address for sessionFor. The MCP layer
// hides the request; the address rides the context instead.
func withCallerIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerIPKey, host)))
	})
}

func (s *Server) ask(ctx context.Context, _ *mcp.CallToolRequest, in AskInput) (*mcp.CallToolResult, AskOutput, error) {
	if strings.TrimSpace(in.Question) == "" {
		return nil, AskOutput{}, fmt.Errorf("question is required")
	}
	q, err := s.createQuestion(ctx, tinyv1.QuestionSpec{
		Text:    in.Question,
		Options: in.Options,
		Session: s.sessionFor(ctx),
		Reason:  ReasonTool,
	})
	if err != nil {
		return nil, AskOutput{}, err
	}
	return s.waitFor(ctx, q.Name)
}

func (s *Server) await(ctx context.Context, _ *mcp.CallToolRequest, in AwaitInput) (*mcp.CallToolResult, AskOutput, error) {
	if in.QuestionID == "" {
		return nil, AskOutput{}, fmt.Errorf("questionId is required")
	}
	return s.waitFor(ctx, in.QuestionID)
}

// attentionRequest is what a hook posts. Everything is optional but the
// message — a Notification hook rarely knows more than "the agent wants you".
type attentionRequest struct {
	Message string `json:"message"`
	Session string `json:"session,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// handleAttention records that a session is visibly waiting on a human. It
// answers immediately (hooks must not hang the agent) and dedupes: one open
// notification per session, its text refreshed, rather than a card per hook
// firing.
func (s *Server) handleAttention(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in attentionRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = "The agent is waiting for your input."
	}

	ctx := r.Context()
	session := s.sessionForIP(ctx, remoteIP(r))
	if in.Session != "" {
		session.Name = in.Session
	}

	// Reuse the open notification for this session if one exists.
	if session.Name != "" {
		list := &tinyv1.QuestionList{}
		if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace), client.MatchingLabels{SessionLabel: session.Name}); err == nil {
			for i := range list.Items {
				q := &list.Items[i]
				if q.Spec.Reason == ReasonNotification && !q.Answered() {
					q.Spec.Text = in.Message
					if err := s.Client.Update(ctx, q); err == nil {
						writeJSON(w, map[string]string{"questionId": q.Name, "deduped": "true"})
						return
					}
				}
			}
		}
	}

	q, err := s.createQuestion(ctx, tinyv1.QuestionSpec{
		Text:    in.Message,
		Session: session,
		Reason:  ReasonNotification,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"questionId": q.Name})
}

func (s *Server) createQuestion(ctx context.Context, spec tinyv1.QuestionSpec) (*tinyv1.Question, error) {
	q := &tinyv1.Question{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "q-",
			Namespace:    s.Namespace,
		},
		Spec: spec,
	}
	if spec.Session.Name != "" {
		q.Labels = map[string]string{SessionLabel: spec.Session.Name}
	}
	if err := s.Client.Create(ctx, q); err != nil {
		return nil, fmt.Errorf("create question: %w", err)
	}
	return q, nil
}

// waitFor blocks until the named question carries an answer, the context ends,
// or the question is gone. An interruption is reported WITH the question id so
// the model resumes with await_answer instead of asking again — asking again
// would put a duplicate card in front of the human.
func (s *Server) waitFor(ctx context.Context, name string) (*mcp.CallToolResult, AskOutput, error) {
	interval := s.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		q := &tinyv1.Question{}
		err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, q)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, AskOutput{}, fmt.Errorf("interrupted while waiting — call await_answer with questionId %q to keep waiting", name)
		case err != nil:
			return nil, AskOutput{}, fmt.Errorf("question %s is gone: %w", name, err)
		case q.Answered():
			return nil, AskOutput{Answer: q.Status.Answer}, nil
		}

		select {
		case <-ctx.Done():
			return nil, AskOutput{}, fmt.Errorf("interrupted while waiting — call await_answer with questionId %q to keep waiting", name)
		case <-t.C:
		}
	}
}

// sessionFor works out which session asked, with zero configuration: the
// caller's pod is found by its IP, and the pod's labels say who owns it. Best
// effort by design — an unattributed question still reaches the human, it
// just renders without a session row to sit under.
func (s *Server) sessionFor(ctx context.Context) tinyv1.SessionRef {
	ip, _ := ctx.Value(callerIPKey).(string)
	return s.sessionForIP(ctx, ip)
}

func (s *Server) sessionForIP(ctx context.Context, ip string) tinyv1.SessionRef {
	if ip == "" || s.Client == nil {
		return tinyv1.SessionRef{}
	}
	pods := &corev1.PodList{}
	if err := s.Client.List(ctx, pods, client.InNamespace(s.Namespace)); err != nil {
		return tinyv1.SessionRef{}
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.PodIP != ip {
			continue
		}
		ref := tinyv1.SessionRef{Pod: p.Name, Kind: "Pod", Name: p.Name}
		// A runner that stamps its pods gets its own identity back. kelos
		// first; anything else falls back to the release/instance label.
		for _, key := range []string{"kelos.dev/session", "app.kubernetes.io/instance"} {
			if v := p.Labels[key]; v != "" {
				ref.Name = v
				ref.Kind = "Session"
				break
			}
		}
		return ref
	}
	return tinyv1.SessionRef{}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
