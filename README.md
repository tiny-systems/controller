# tiny controller

The human gate for unattended coding agents on Kubernetes.

An agent working alone eventually reaches a decision it must not make by
itself — force-push, drop a table, spend real money. This repo provides the
smallest possible mechanism for that moment:

- **One CRD: `Question`.** A parked request for a human decision. `kubectl
  get questions` shows who is waiting, on what, and what was answered.
- **One deployment: `tiny-human`.** An MCP server exposing `ask_human` — the
  call **blocks** (minutes or hours) until a person writes the answer into the
  Question's status, then returns it to the agent. Plus `/attention`, a
  safety-net endpoint for [Claude Code hooks](https://docs.claude.com) so a
  session that ignored the tool and simply asked in its shell still lights up.

It is runner-agnostic on purpose: [kelos](https://github.com/kelos-dev/kelos)
sessions, plain pods, anything that can reach the Service gets the same gate.
Point your agent's MCP config at:

```
http://tiny-human.<namespace>.svc:8080/mcp
```

Answer with any client — the `tiny` TUI ([tiny-systems/client](https://github.com/tiny-systems/client)),
or plainly:

```sh
kubectl get questions
kubectl patch question q-xxxxx --type=merge --subresource=status \
  -p '{"status":{"answer":"yes","answeredBy":"me"}}'
```

## Install

```sh
kubectl apply -f config/crd/bases/agents.tinysystems.io_questions.yaml
kubectl apply -n <namespace> -f config/deploy/tiny-human.yaml
```

> 🌱 Early and building in the open. Part of the
> [tiny systems](https://tinysystems.io) session tooling.
