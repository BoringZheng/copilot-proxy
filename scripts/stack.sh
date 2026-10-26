#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
task_action=${1:-help}
shift || true
case "$task_action" in
  init)
    command -v openssl >/dev/null
    umask 077
    [[ ! -L .env && ! -L secrets ]]
    if [[ ! -e .env ]]; then cp .env.example .env; fi
    mkdir -p secrets
    chmod 700 secrets
    for task_name in session_secret crypto_secret; do
      task_file="secrets/$task_name"
      [[ ! -L "$task_file" ]]
      if [[ ! -e "$task_file" ]]; then
        (set -o noclobber; openssl rand -hex 32 > "$task_file")
      fi
      [[ -f "$task_file" && $(wc -c < "$task_file") -ge 64 ]]
      chmod 600 "$task_file"
    done
    printf 'Initialized local settings and secrets; existing values were retained.\n'
    ;;
  build)
    docker compose --profile secondary build "$@"
    ;;
  auth)
    task_account=${1:-}
    [[ "$task_account" == primary || "$task_account" == secondary ]] || { echo 'Usage: bash scripts/stack.sh auth primary|secondary' >&2; exit 2; }
    task_service="copilot-$task_account"
    if [[ -n "$(docker compose --profile secondary ps --status running -q "$task_service")" ]]; then
      echo "Stop $task_service explicitly before changing its login; active requests would be interrupted." >&2
      exit 1
    fi
    docker compose --profile secondary run --rm --no-deps "$task_service" auth
    ;;
  up)
    if [[ "${1:-}" == --secondary ]]; then
      shift
      docker compose --profile secondary up -d "$@"
    else
      docker compose up -d "$@"
    fi
    ;;
  status)
    docker compose --profile secondary ps
    ;;
  down)
    # Never remove volumes here: credentials and the New API database live there.
    [[ $# == 0 ]] || { echo 'down accepts no flags; volumes are deliberately retained.' >&2; exit 2; }
    docker compose --profile secondary down
    ;;
  *)
    echo 'Usage: bash scripts/stack.sh init|build|auth primary|auth secondary|up [--secondary]|status|down'
    [[ "$task_action" == help ]]
    ;;
esac

