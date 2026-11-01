# The sandbox that runs Codex commands for a task (ADR-0016). Build it with
# `just build-e2b-codex-template`. Bubblewrap is required: without it Codex
# cannot enforce workspace-write, and approvals lose their meaning. The adapter
# starts exec-server after create, because a template's start command is captured
# at build time and cannot hold a per-sandbox token hash.
FROM e2bdev/base@sha256:3638f4332f0a8517bebcff5ebf149c1909263cd8fd77c0eff3a42b7d8ab7f47c

ARG CODEX_VERSION=0.165.0
ARG CODEX_ARCHIVE=codex-x86_64-unknown-linux-musl
ARG CODEX_SHA256=be635d3486a5bd060d0fff582ae6d30160280c31344dd85719707fa920e187f0

USER root
RUN apt-get update -qq && apt-get install -y -qq bubblewrap && bwrap --version
RUN set -e; \
    curl --proto '=https' --proto-redir '=https' -fL --retry 3 -o /tmp/codex.tar.gz \
      "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/${CODEX_ARCHIVE}.tar.gz"; \
    echo "${CODEX_SHA256}  /tmp/codex.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/codex.tar.gz -C /usr/local/bin; \
    mv "/usr/local/bin/${CODEX_ARCHIVE}" /usr/local/bin/codex; \
    chmod 0755 /usr/local/bin/codex; \
    rm /tmp/codex.tar.gz; \
    test "$(codex --version)" = "codex-cli ${CODEX_VERSION}"; \
    mkdir -p /workspace; \
    chown user:user /workspace
USER user
