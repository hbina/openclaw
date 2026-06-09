FROM node:24-bookworm

RUN apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl git openssh-server procps python3 sudo tini && \
    rm -rf /var/lib/apt/lists/* && \
    corepack enable && \
    mkdir -p /run/sshd /workspace

WORKDIR /workspace/openclaw
COPY . /workspace/openclaw
COPY scripts/docker/manual-ssh-entrypoint.sh /usr/local/bin/openclaw-manual-ssh-entrypoint

RUN pnpm install --frozen-lockfile && \
    chmod 755 /usr/local/bin/openclaw-manual-ssh-entrypoint && \
    chown -R node:node /workspace

EXPOSE 22 18789

ENTRYPOINT ["tini", "--", "/usr/local/bin/openclaw-manual-ssh-entrypoint"]
