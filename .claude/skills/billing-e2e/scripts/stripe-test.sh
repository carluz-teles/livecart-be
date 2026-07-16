#!/usr/bin/env bash
# Helper para a API da Stripe em TEST MODE (billing-e2e).
# Usa STRIPE_TEST_KEY do ambiente ou o STRIPE_SECRET_KEY do .env local do BE.
# Recusa qualquer chave que nao seja sk_test_ — jamais rodar contra live.
set -euo pipefail

KEY="${STRIPE_TEST_KEY:-$(grep '^STRIPE_SECRET_KEY=' /home/carluz_teles/livecart-be/.env | cut -d= -f2)}"
if [[ "$KEY" != sk_test_* ]]; then
  echo "ERRO: chave nao e sk_test_ — abortando (nunca usar live no E2E)." >&2
  exit 1
fi

api() { curl -sS -u "$KEY:" "$@"; }

cmd="${1:-help}"; shift || true
case "$cmd" in
  sub)            # sub <sub_id> — estado resumido da subscription
    api "https://api.stripe.com/v1/subscriptions/$1" | python3 -c '
import json,sys; s=json.load(sys.stdin)
print(json.dumps({"id":s["id"],"status":s["status"],"trial_end":s.get("trial_end"),
 "current_period_end":s.get("current_period_end"),
 "default_payment_method":s.get("default_payment_method"),
 "items":[{"price":i["price"]["id"],"nickname":i["price"].get("nickname")} for i in s["items"]["data"]],
 "cancel_at_period_end":s.get("cancel_at_period_end")}, indent=1))' ;;
  expire-trial)   # expire-trial <sub_id> — encerra o trial AGORA (sem cartao => paused)
    api "https://api.stripe.com/v1/subscriptions/$1" -d "trial_end=now" | python3 -c '
import json,sys; s=json.load(sys.stdin); print(s.get("id"), "->", s.get("status"), s.get("error",""))' ;;
  invoices)       # invoices <customer_id> — ultimas faturas
    api "https://api.stripe.com/v1/invoices?customer=$1&limit=5" | python3 -c '
import json,sys
for i in json.load(sys.stdin)["data"]:
    print(i["id"], i["status"], i["amount_due"], i.get("billing_reason"))' ;;
  events)         # events [tipo] — ultimos eventos test-mode (debug de webhook)
    api "https://api.stripe.com/v1/events?limit=10${1:+&type=$1}" | python3 -c '
import json,sys
for e in json.load(sys.stdin)["data"]:
    print(e["id"], e["type"])' ;;
  cancel)         # cancel <sub_id> — cancela a subscription (cleanup)
    api -X DELETE "https://api.stripe.com/v1/subscriptions/$1" | python3 -c '
import json,sys; s=json.load(sys.stdin); print(s.get("id"), "->", s.get("status"))' ;;
  del-customer)   # del-customer <cus_id> — apaga o customer de teste (cleanup)
    api -X DELETE "https://api.stripe.com/v1/customers/$1" ;;
  *)
    grep -E '^  [a-z-]+\)' "$0" | sed 's/)#*/ —/' ;;
esac
