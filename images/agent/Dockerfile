# The tiny agent image: Claude Code + tmux + git, driven by entrypoint.sh.
# Deliberately small surface — the sidecar owns cluster powers; this image
# owns only how the agent starts, resumes, and reports attention.
FROM node:22-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      git curl ca-certificates tmux ripgrep procps \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g @anthropic-ai/claude-code

RUN useradd -m -u 61000 agent && mkdir -p /workspace && chown agent /workspace
COPY entrypoint.sh /usr/local/bin/tiny-entrypoint
USER agent
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/tiny-entrypoint"]
