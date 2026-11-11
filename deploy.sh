#!/usr/bin/env bash
# Deploy megabot on a server with Docker, over ssh. Settings in deploy.env (see deploy.env.example).
#   ./deploy.sh copia     build the image here and copy it, docker-compose.yml and .env to the server (does not start)
#   ./deploy.sh avvia     disable the n8n "Openai workflow" and start megabot
#   ./deploy.sh log       last lines of the log
#   ./deploy.sh rollback  stop megabot and reactivate the n8n workflow
set -euo pipefail
cd "$(dirname "$0")"

[ -f deploy.env ] || { echo "Manca deploy.env: cp deploy.env.example deploy.env"; exit 1; }
# shellcheck source=/dev/null
. ./deploy.env
: "${HOST:?HOST mancante in deploy.env}"
DIR=${DIR:-megabot}
IMAGE=${IMAGE:-dangerblack/megabot:latest}

# one connection for the whole script: the password is asked only once
SOCK=$(mktemp -u /tmp/megabot-ssh.XXXX)
SSH=(ssh -o ControlMaster=auto -o ControlPath="$SOCK" -o ControlPersist=60)
trap '"${SSH[@]}" -O exit "$HOST" 2>/dev/null || true' EXIT
remote() { "${SSH[@]}" "$HOST" "cd $DIR && DC=\$(docker compose version >/dev/null 2>&1 && echo 'docker compose' || echo docker-compose) && $1"; }

# optional: the n8n workflow that used the same Telegram token (off on start, on again on rollback)
n8n() { # n8n <activate|deactivate>
  [ -n "${N8N_URL:-}" ] && [ -n "${N8N_WORKFLOW_ID:-}" ] || return 0
  local key; key=$(grep -E '^N8N_API_KEY=' "$N8N_KEY_FILE" | cut -d= -f2- | tr -d '"')
  echo "n8n: $1 workflow $N8N_WORKFLOW_ID"
  curl -fsS -X POST -H "X-N8N-API-KEY: $key" "$N8N_URL/api/v1/workflows/$N8N_WORKFLOW_ID/$1" | grep -o '"active":[a-z]*' || true
}

case "${1:-}" in
copia)
  [ -f .env ] || { echo "Manca .env: cp .env.example .env e compilalo"; exit 1; }
  for v in TOKEN ROOT_USER_ID; do
    grep -qE "^$v=.+" .env || { echo "In .env manca $v"; exit 1; }
  done
  docker build -t "$IMAGE" .
  "${SSH[@]}" "$HOST" "mkdir -p $DIR/data"
  scp -o ControlPath="$SOCK" docker-compose.yml .env "$HOST:$DIR/"
  "${SSH[@]}" "$HOST" "chmod 600 $DIR/.env"
  echo "Copio l'immagine (~15 MB compressa)…"
  docker save "$IMAGE" | gzip | "${SSH[@]}" "$HOST" "gunzip | docker load"
  echo "Fatto. Quando sei pronto: ./deploy.sh avvia"
  ;;
avvia)
  n8n deactivate
  remote '$DC up -d --no-build && sleep 5 && $DC logs --tail 20'
  echo "Scrivi /start al bot. Se qualcosa non va: ./deploy.sh rollback"
  ;;
log)
  remote '$DC logs --tail 50'
  ;;
rollback)
  remote '$DC stop'
  n8n activate
  ;;
*)
  sed -n '2,6p' "$0"; exit 1
  ;;
esac
