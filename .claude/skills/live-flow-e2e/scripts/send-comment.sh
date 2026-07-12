#!/usr/bin/env bash
# Simula um comentário de live do Instagram via webhook.
#
# Uso:
#   API_BASE=http://localhost:3001 ./send-comment.sh <media_id> <texto> [user_id] [username] [comment_id]
#
# Exemplos:
#   ./send-comment.sh SIM-1720000000 "9432"            sim_user_1 maria_teste
#   ./send-comment.sh SIM-1720000000 "quero 3 9432"    sim_user_2 joao_teste
#
# comment_id é gerado único por padrão (guard de idempotência no backend);
# passe o quinto argumento apenas para testar a idempotência.
set -euo pipefail

API_BASE="${API_BASE:-http://localhost:3001}"
MEDIA_ID="${1:?uso: send-comment.sh <media_id> <texto> [user_id] [username] [comment_id]}"
TEXT="${2:?informe o texto do comentário}"
USER_ID="${3:-sim_user_1}"
USERNAME="${4:-comprador_teste}"
COMMENT_ID="${5:-sim-comment-$(date +%s%N)}"

PAYLOAD=$(cat <<JSON
{
  "object": "instagram",
  "entry": [
    {
      "id": "sim-account-1",
      "time": $(date +%s),
      "changes": [
        {
          "field": "live_comments",
          "value": {
            "from": {
              "id": "${USER_ID}",
              "username": "${USERNAME}",
              "self_ig_scoped_id": "igsid_${USER_ID}"
            },
            "id": "${COMMENT_ID}",
            "text": "${TEXT}",
            "media": {
              "id": "${MEDIA_ID}",
              "media_product_type": "IG_LIVE"
            }
          }
        }
      ]
    }
  ]
}
JSON
)

echo "→ comment_id=${COMMENT_ID} user=@${USERNAME} text=\"${TEXT}\" media=${MEDIA_ID}"
curl -sS -X POST "${API_BASE}/api/webhooks/instagram" \
  -H "Content-Type: application/json" \
  -d "${PAYLOAD}"
echo
