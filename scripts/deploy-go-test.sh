#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_DIR="$ROOT_DIR/golang"
CONFIG_FILE="$ROOT_DIR/config_test/openclaw.json"
SECRETS_FILE="$ROOT_DIR/config_test/secrets.json"
DATA_DIR="$ROOT_DIR/config_test/agent_data_go"
DB_FILE="$DATA_DIR/openclaw-agent.sqlite"

CONTAINER="openclaw-go-test-ubuntu"
IMAGE_REPOSITORY="openclaw-go-ubuntu-test"
HOST_PORT="18792"
CONTAINER_PORT="18789"
CHAT_HEALTH_URL="http://172.17.0.1:8080/health"
EMBEDDING_HEALTH_URL="http://172.17.0.1:8081/health"
GATEWAY_HEALTH_URL="http://127.0.0.1:${HOST_PORT}/healthz"
EXPECTED_HEALTH='{"status":"ok"}'
REQUESTED_REPLY="manual deploy healthy"

DEPLOY_ID="$(date -u +%Y%m%dT%H%M%SZ)"
IMAGE_TAG="manual-$DEPLOY_ID"

deployment_started=0
old_stopped=0
deployment_verified=0
backup_container=""

usage() {
  cat <<'EOF'
Usage: scripts/deploy-go-test.sh [--tag TAG]

Build, verify, and safely replace the persistent Go test deployment.
The current container is retained for automatic rollback until all live proof
passes. The SQLite backup is intentionally retained after a successful deploy.
This makes the deployment procedure repeatable; reproducible image bytes still
require pinning the Dockerfile's base images and Alpine package inputs.
The live model may vary its wording; smoke proof requires a successful,
non-empty persisted assistant reply rather than byte-identical prose.

Options:
  --tag TAG  Use IMAGE_REPOSITORY:TAG instead of a UTC timestamp tag.
  -h, --help Show this help.
EOF
}

step() {
  printf '\n==> %s\n' "$*"
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

container_exists() {
  docker container inspect "$1" >/dev/null 2>&1
}

wait_for_gateway() {
  local attempt
  for attempt in {1..30}; do
    if curl -fsS "$GATEWAY_HEALTH_URL" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

rollback_on_failure() {
  local status=$?
  trap - EXIT

  if ((status == 0 || deployment_started == 0 || deployment_verified == 1)); then
    exit "$status"
  fi

  printf '\nDeployment failed; attempting rollback.\n' >&2
  set +e

  if container_exists "$backup_container"; then
    if container_exists "$CONTAINER"; then
      docker logs --tail 80 "$CONTAINER" >&2
      docker rm -f "$CONTAINER" >/dev/null
    fi
    docker rename "$backup_container" "$CONTAINER"
    docker start "$CONTAINER" >/dev/null
  elif ((old_stopped == 1)) && container_exists "$CONTAINER"; then
    docker start "$CONTAINER" >/dev/null
  fi

  if wait_for_gateway; then
    printf 'Rollback health check passed: %s\n' "$GATEWAY_HEALTH_URL" >&2
  else
    printf 'Rollback completed, but Gateway health did not recover.\n' >&2
  fi

  exit "$status"
}

trap rollback_on_failure EXIT

while (($# > 0)); do
  case "$1" in
    --tag)
      (($# >= 2)) || fail "--tag requires a value"
      IMAGE_TAG="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ "$IMAGE_TAG" =~ ^[A-Za-z0-9_.-]+$ ]] ||
  fail "tag must contain only letters, digits, periods, underscores, and hyphens"

IMAGE="$IMAGE_REPOSITORY:$IMAGE_TAG"
backup_container="${CONTAINER}-before-${DEPLOY_ID}"
DB_BACKUP="${DB_FILE}.before-${DEPLOY_ID}"
SMOKE_SENDER="manual-deploy-${DEPLOY_ID}"

for command_name in curl docker git go gofmt sqlite3; do
  require_command "$command_name"
done

[[ -f "$CONFIG_FILE" ]] || fail "missing config: $CONFIG_FILE"
[[ -f "$SECRETS_FILE" ]] || fail "missing secrets file: $SECRETS_FILE"
[[ -d "$DATA_DIR" ]] || fail "missing data directory: $DATA_DIR"
[[ -f "$DB_FILE" ]] || fail "missing SQLite database: $DB_FILE"
container_exists "$CONTAINER" || fail "container does not exist: $CONTAINER"
container_exists "$backup_container" &&
  fail "rollback container already exists: $backup_container"
if docker image inspect "$IMAGE" >/dev/null 2>&1; then
  fail "image tag already exists; choose a unique tag: $IMAGE"
fi

step "Inspecting source and deployment"
git -C "$ROOT_DIR" status -sb
git -C "$ROOT_DIR" diff --check
source_revision="$(git -C "$ROOT_DIR" rev-parse HEAD)"
if [[ -n "$(git -C "$ROOT_DIR" status --porcelain -- golang)" ]]; then
  source_state="dirty"
else
  source_state="clean"
fi

[[ "$(docker inspect "$CONTAINER" --format '{{.State.Running}}')" == "true" ]] ||
  fail "container is not running: $CONTAINER"
[[ "$(docker inspect "$CONTAINER" --format '{{.HostConfig.RestartPolicy.Name}}')" == "unless-stopped" ]] ||
  fail "unexpected restart policy on $CONTAINER"
[[ "$(docker inspect "$CONTAINER" --format '{{.HostConfig.NetworkMode}}')" == "bridge" ]] ||
  fail "unexpected network mode on $CONTAINER"
mount_count="$(docker inspect "$CONTAINER" --format '{{len .Mounts}}')"
[[ "$mount_count" == "3" || "$mount_count" == "4" ]] ||
  fail "expected three canonical mounts, or four before removal of the stale /app/data volume"
[[ "$(docker inspect "$CONTAINER" --format '{{len .Config.Env}}')" == "4" ]] ||
  fail "expected exactly four container environment entries"
[[ "$(docker inspect "$CONTAINER" --format '{{len .HostConfig.PortBindings}}')" == "1" ]] ||
  fail "expected exactly one published container port"

current_config_source="$(docker inspect "$CONTAINER" --format \
  '{{range .Mounts}}{{if eq .Destination "/config/openclaw.json"}}{{.Source}}{{end}}{{end}}')"
current_secrets_source="$(docker inspect "$CONTAINER" --format \
  '{{range .Mounts}}{{if eq .Destination "/config/secrets.json"}}{{.Source}}{{end}}{{end}}')"
current_data_source="$(docker inspect "$CONTAINER" --format \
  '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}')"
published_port="$(docker inspect "$CONTAINER" --format \
  '{{with (index .HostConfig.PortBindings "18789/tcp")}}{{(index . 0).HostIp}}:{{(index . 0).HostPort}}{{end}}')"
config_mount_rw="$(docker inspect "$CONTAINER" --format \
  '{{range .Mounts}}{{if eq .Destination "/config/openclaw.json"}}{{.RW}}{{end}}{{end}}')"
secrets_mount_rw="$(docker inspect "$CONTAINER" --format \
  '{{range .Mounts}}{{if eq .Destination "/config/secrets.json"}}{{.RW}}{{end}}{{end}}')"
data_mount_rw="$(docker inspect "$CONTAINER" --format \
  '{{range .Mounts}}{{if eq .Destination "/data"}}{{.RW}}{{end}}{{end}}')"

[[ "$current_config_source" == "$CONFIG_FILE" ]] ||
  fail "unexpected /config/openclaw.json source: $current_config_source"
[[ "$current_secrets_source" == "$SECRETS_FILE" ]] ||
  fail "unexpected /config/secrets.json source: $current_secrets_source"
[[ "$current_data_source" == "$DATA_DIR" ]] ||
  fail "unexpected /data source: $current_data_source"
[[ "$published_port" == "0.0.0.0:$HOST_PORT" ]] ||
  fail "unexpected published Gateway port: $published_port"
[[ "$config_mount_rw" == "false" ]] ||
  fail "/config/openclaw.json must be read-only"
[[ "$secrets_mount_rw" == "false" ]] ||
  fail "/config/secrets.json must be read-only"
[[ "$data_mount_rw" == "true" ]] || fail "/data must be writable"

container_env="$(docker inspect "$CONTAINER" --format '{{range .Config.Env}}{{println .}}{{end}}')"
for expected_env in \
  "OPENCLAW_DATA_DIR=/data" \
  "OPENCLAW_CONFIG_DIR=/config" \
  "TZ=Asia/Kuala_Lumpur"; do
  [[ $'\n'"$container_env"$'\n' == *$'\n'"$expected_env"$'\n'* ]] ||
    fail "missing expected container environment: $expected_env"
done

printf 'Current image: %s\n' "$(docker inspect "$CONTAINER" --format '{{.Config.Image}}')"
printf 'Next image:    %s\n' "$IMAGE"

step "Running Go verification gates"
(
  cd "$GO_DIR"
  GOCACHE=/tmp/openclaw-go-cache go test ./...
  GOCACHE=/tmp/openclaw-go-cache go test -race ./...
  GOCACHE=/tmp/openclaw-go-cache go vet ./...
  GOCACHE=/tmp/openclaw-go-cache go build -o /tmp/openclaw-go ./cmd/openclaw

  unformatted="$(gofmt -l .)"
  [[ -z "$unformatted" ]] || {
    printf 'gofmt is required for:\n%s\n' "$unformatted" >&2
    exit 1
  }
)
git -C "$ROOT_DIR" diff --check

step "Checking local model health"
[[ "$(curl -fsS "$CHAT_HEALTH_URL")" == "$EXPECTED_HEALTH" ]] ||
  fail "chat llama-server health check failed"
[[ "$(curl -fsS "$EMBEDDING_HEALTH_URL")" == "$EXPECTED_HEALTH" ]] ||
  fail "embedding llama-server health check failed"

step "Backing up SQLite"
[[ ! -e "$DB_BACKUP" ]] || fail "backup already exists: $DB_BACKUP"
sqlite3 "$DB_FILE" ".backup '$DB_BACKUP'"
[[ "$(sqlite3 "$DB_BACKUP" 'PRAGMA integrity_check;')" == "ok" ]] ||
  fail "SQLite backup integrity check failed"
printf 'SQLite backup: %s\n' "$DB_BACKUP"

step "Building $IMAGE"
docker build -t "$IMAGE" "$GO_DIR"
image_id="$(docker image inspect "$IMAGE" --format '{{.Id}}')"
running_image_id="$(docker inspect "$CONTAINER" --format '{{.Image}}')"
printf 'Built image id:   %s\n' "$image_id"
printf 'Running image id: %s\n' "$running_image_id"

step "Replacing $CONTAINER"
deployment_started=1
old_stopped=1
docker stop "$CONTAINER"
docker rename "$CONTAINER" "$backup_container"

docker run -d \
  --name "$CONTAINER" \
  --restart unless-stopped \
  --network bridge \
  -p "0.0.0.0:${HOST_PORT}:${CONTAINER_PORT}" \
  -e OPENCLAW_DATA_DIR=/data \
  -e OPENCLAW_CONFIG_DIR=/config \
  -e TZ=Asia/Kuala_Lumpur \
  -v "$CONFIG_FILE:/config/openclaw.json:ro" \
  -v "$SECRETS_FILE:/config/secrets.json:ro" \
  -v "$DATA_DIR:/data" \
  "$IMAGE" >/dev/null

if ! wait_for_gateway; then
  docker logs --tail 80 "$CONTAINER" >&2
  fail "Gateway did not become healthy"
fi

step "Checking model access from the new container"
[[ "$(docker exec "$CONTAINER" wget -qO- "$CHAT_HEALTH_URL")" == "$EXPECTED_HEALTH" ]] ||
  fail "new container cannot reach the chat llama-server"
[[ "$(docker exec "$CONTAINER" wget -qO- "$EMBEDDING_HEALTH_URL")" == "$EXPECTED_HEALTH" ]] ||
  fail "new container cannot reach the embedding llama-server"

step "Running a real-model smoke turn"
smoke_response="$(curl -fsS \
  -H 'Content-Type: application/json' \
  -d "{\"sender_id\":\"$SMOKE_SENDER\",\"message\":\"Reply with exactly: $REQUESTED_REPLY\"}" \
  "http://127.0.0.1:${HOST_PORT}/chat")"
[[ "$smoke_response" == '{"reply":'* ]] ||
  fail "unexpected smoke response: $smoke_response"
[[ "$(sqlite3 "$DB_FILE" \
  "SELECT count(*) FROM conversation_history WHERE channel_id='cli' AND sender_id='$SMOKE_SENDER';")" == "2" ]] ||
  fail "expected exactly two persisted smoke transcript rows"
assistant_reply="$(sqlite3 "$DB_FILE" \
  "SELECT content FROM conversation_history WHERE channel_id='cli' AND sender_id='$SMOKE_SENDER' AND role='assistant' AND content_type='text' ORDER BY id DESC LIMIT 1;")"
[[ -n "$assistant_reply" ]] || fail "smoke turn did not persist a non-empty assistant reply"
[[ "$(sqlite3 "$DB_FILE" 'PRAGMA integrity_check;')" == "ok" ]] ||
  fail "SQLite integrity check failed after smoke turn"

step "Verifying restart persistence"
docker restart "$CONTAINER" >/dev/null
wait_for_gateway || fail "Gateway did not recover after restart"
[[ "$(sqlite3 "$DB_FILE" \
  "SELECT count(*) FROM conversation_history WHERE channel_id='cli' AND sender_id='$SMOKE_SENDER';")" == "2" ]] ||
  fail "smoke transcript did not persist across restart"
[[ "$(sqlite3 "$DB_FILE" 'PRAGMA integrity_check;')" == "ok" ]] ||
  fail "SQLite integrity check failed after restart"

deployment_verified=1

step "Removing the stopped rollback container"
docker rm "$backup_container" >/dev/null

container_id="$(docker inspect "$CONTAINER" --format '{{.Id}}')"
transcript_count="$(sqlite3 "$DB_FILE" 'SELECT count(*) FROM conversation_history;')"
reminder_count="$(sqlite3 "$DB_FILE" 'SELECT count(*) FROM reminders;')"
memory_count="$(sqlite3 "$DB_FILE" 'SELECT count(*) FROM memory_entries;')"
chunk_count="$(sqlite3 "$DB_FILE" 'SELECT count(*) FROM conversation_chunks;')"

step "Deployment complete"
printf 'Container:       %s\n' "$CONTAINER"
printf 'Container id:    %s\n' "$container_id"
printf 'Image:           %s\n' "$IMAGE"
printf 'Image id:        %s\n' "$image_id"
printf 'Source revision: %s (%s golang tree)\n' "$source_revision" "$source_state"
printf 'Gateway:         %s\n' "$GATEWAY_HEALTH_URL"
printf 'Smoke sender:    %s\n' "$SMOKE_SENDER"
printf 'Smoke response:  %s\n' "$smoke_response"
printf 'SQLite backup:   %s\n' "$DB_BACKUP"
printf 'State counts:    %s transcripts, %s reminders, %s memories, %s chunks\n' \
  "$transcript_count" "$reminder_count" "$memory_count" "$chunk_count"
printf '\nRecord this deployment evidence in the repo-root AGENTS.md.\n'
