#!/usr/bin/env bash
# Consulta os e-mails enviados pela plataforma via API do Resend (staging).
# Chave: RESEND_API_KEY do ambiente, ou lida das vars de staging no Railway.
# Uso:
#   resend-emails.sh list [N]        — últimos N e-mails (default 10)
#   resend-emails.sh get <email_id>  — conteúdo completo (inclui html)
#   resend-emails.sh html <email_id> — só o HTML (para abrir/screenshotar)
set -euo pipefail

if [ -z "${RESEND_API_KEY:-}" ]; then
  RW=$(ls /home/carluz_teles/.asdf/installs/nodejs/*/bin/railway 2>/dev/null | head -1)
  RESEND_API_KEY=$(cd /home/carluz_teles/livecart-be && "$RW" variables --environment staging --service livecart-be --kv 2>/dev/null | grep '^RESEND_API_KEY=' | cut -d= -f2)
fi
[ -z "$RESEND_API_KEY" ] && { echo "RESEND_API_KEY não encontrada" >&2; exit 1; }

api() { curl -s -H "Authorization: Bearer $RESEND_API_KEY" "$@"; }

cmd="${1:-list}"; shift || true
case "$cmd" in
  list)
    api "https://api.resend.com/emails?limit=${1:-10}" | python3 -c '
import json,sys
for e in json.load(sys.stdin).get("data",[]):
    print(e["id"], "|", e.get("created_at","")[:19], "|", e.get("last_event","?"), "|", e.get("to"), "|", e.get("subject"))' ;;
  get)
    api "https://api.resend.com/emails/$1" | python3 -m json.tool | head -40 ;;
  html)
    api "https://api.resend.com/emails/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("html",""))' ;;
  *) echo "uso: list [N] | get <id> | html <id>" >&2; exit 1 ;;
esac
