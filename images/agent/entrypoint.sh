#!/bin/bash
# tiny agent entrypoint — Claude Code in tmux, resumable by design.
#
# Contract with the Session controller:
#   TINY_TASK          the task (first prompt)
#   TINY_REPO          optional git URL cloned into the workspace on first run
#   TINY_SESSION_NAME  identity, for logs only (the sidecar carries the real one)
#   /workspace         the persistent volume; everything that matters lives here
#
# Resume rule: the FIRST pod for a session starts claude with the task; any
# later pod finds the transcript in the workspace and continues it instead —
# a rescheduled pod is a continuation, never a restart from zero.
set -uo pipefail

WORKSPACE=/workspace
MARKER="$WORKSPACE/.tiny/started"
export CLAUDE_CONFIG_DIR="$WORKSPACE/.claude"
mkdir -p "$WORKSPACE/.tiny" "$CLAUDE_CONFIG_DIR"

# Seed claude's user config: onboarding done (no human at the theme picker),
# the workspace pre-trusted (no human at the trust dialog), and the tiny MCP
# server registered USER-SCOPE — this is the file MCP servers live in;
# settings.json below carries only hooks. Written only when absent so later
# runs keep whatever state claude accumulated.
if [ ! -f "$CLAUDE_CONFIG_DIR/.claude.json" ]; then
  cat > "$CLAUDE_CONFIG_DIR/.claude.json" <<'JSON'
{
  "hasCompletedOnboarding": true,
  "theme": "dark",
  "mcpServers": {
    "tiny": { "type": "http", "url": "http://127.0.0.1:8080/mcp" }
  },
  "projects": {
    "/workspace":      { "hasTrustDialogAccepted": true },
    "/workspace/repo": { "hasTrustDialogAccepted": true }
  }
}
JSON
fi

# The attention hooks — the safety net when the model asks in the shell
# instead of calling ask_human. Written every start so an image upgrade can
# evolve the wiring; user settings merge over.
cat > "$CLAUDE_CONFIG_DIR/settings.json" <<'JSON'
{
  "permissions": {
    "allow": ["mcp__tiny__ask_human", "mcp__tiny__await_answer", "mcp__tiny__session_list", "mcp__tiny__session_create", "mcp__tiny__expose_port"]
  },
  "hooks": {
    "Notification": [
      { "hooks": [ { "type": "command",
          "command": "curl -s -m 5 -X POST http://127.0.0.1:8080/attention -H 'content-type: application/json' -d \"{\\\"message\\\": \\\"The agent is waiting for your input.\\\"}\" >/dev/null" } ] }
    ],
    "Stop": [
      { "hooks": [ { "type": "command",
          "command": "curl -s -m 5 -X POST http://127.0.0.1:8080/attention -H 'content-type: application/json' -d \"{\\\"message\\\": \\\"The agent finished its turn.\\\", \\\"reason\\\": \\\"stop\\\"}\" >/dev/null" } ] }
    ]
  }
}
JSON

# First run: bring the repo in, if there is one.
if [ ! -f "$MARKER" ] && [ -n "${TINY_REPO:-}" ]; then
  echo "tiny: cloning $TINY_REPO"
  git clone --depth 1 "$TINY_REPO" "$WORKSPACE/repo" || echo "tiny: clone failed — starting on an empty workspace"
fi
cd "$WORKSPACE/repo" 2>/dev/null || cd "$WORKSPACE"

# The agent runs inside tmux so a human can attach mid-thought and detach
# without stopping anything. tmux execs its argument directly (no shell), so
# the command lives in a script — one argument, shell semantics preserved.
RUN="$WORKSPACE/.tiny/run.sh"
if [ -f "$MARKER" ]; then
  echo "tiny: workspace has history — resuming"
  cat > "$RUN" <<'RUNEOF'
#!/bin/bash
claude --continue --permission-mode acceptEdits
echo
echo "tiny: agent exited — session stays for inspection"
sleep infinity
RUNEOF
else
  date -u +%FT%TZ > "$MARKER"
  echo "tiny: fresh session — starting task"
  cat > "$RUN" <<RUNEOF
#!/bin/bash
claude --permission-mode acceptEdits "\${TINY_TASK:-Introduce yourself and wait for instructions.}"
echo
echo "tiny: agent exited — session stays for inspection"
sleep infinity
RUNEOF
fi
chmod +x "$RUN"

# Detached: a pod has no TTY. A human attaches later with
#   kubectl exec -it <pod> -c agent -- tmux attach -t main
tmux new-session -d -s main "$RUN" || { echo "tiny: tmux failed to start"; exit 1; }

# PID 1 lives as long as the tmux session does.
while tmux has-session -t main 2>/dev/null; do sleep 5; done
