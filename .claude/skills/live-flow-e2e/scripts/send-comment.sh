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
#
# Assinatura do webhook: exporte INSTAGRAM_APP_SECRET com o MESMO valor que a
# API usa e o script assina o corpo em X-Hub-Signature-256. Sem a variável ele
# envia sem assinar, o que só funciona enquanto a API estiver em modo
# observação (INSTAGRAM_WEBHOOK_ENFORCE_SIGNATURE != true). Com a aplicação
# ligada, comentário sem assinatura recebe 401.
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

SIG_HEADER=()
if [[ -n "${INSTAGRAM_APP_SECRET:-}" ]]; then
  # Precisa ser o HMAC do corpo EXATO que vai no -d, byte a byte.
  DIGEST=$(printf '%s' "${PAYLOAD}" | openssl dgst -sha256 -hmac "${INSTAGRAM_APP_SECRET}" -r | cut -d' ' -f1)
  SIG_HEADER=(-H "X-Hub-Signature-256: sha256=${DIGEST}")
  echo "→ assinado (X-Hub-Signature-256)"
else
  echo "→ SEM assinatura — só funciona com a API em modo observação"
fi

echo "→ comment_id=${COMMENT_ID} user=@${USERNAME} text=\"${TEXT}\" media=${MEDIA_ID}"
curl -sS -X POST "${API_BASE}/api/webhooks/instagram" \
  -H "Content-Type: application/json" \
  "${SIG_HEADER[@]}" \
  -d "${PAYLOAD}"
echo
