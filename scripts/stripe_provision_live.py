#!/usr/bin/env python3
"""Provisiona o billing do LiveCart no Stripe em LIVE mode (PRD 007).

Replica o setup existente em test mode: meter gmv_cents, 3 products
(Start/Grow/Scale), 6 prices (flat licensed + GMV metered, BRL/mensal) e o
webhook endpoint de producao. Idempotente: reaproveita recursos existentes
(meter por event_name, product por metadata.plan, price por nickname,
webhook por URL).

Uso:
    STRIPE_LIVE_SECRET_KEY=sk_live_... python3 scripts/stripe_provision_live.py

Ao final imprime o bloco de variaveis STRIPE_* para o Railway (production).
O whsec_ do webhook so aparece na criacao — se o endpoint ja existir, o
secret precisa ser lido no dashboard.
"""

import json
import os
import sys
import urllib.parse
import urllib.request

API = "https://api.stripe.com/v1"
KEY = os.environ.get("STRIPE_LIVE_SECRET_KEY", "")

WEBHOOK_URL = "https://api.livecart.com.br/api/webhooks/stripe"
WEBHOOK_EVENTS = [
    "checkout.session.completed",
    "customer.subscription.created",
    "customer.subscription.updated",
    "customer.subscription.deleted",
    "customer.subscription.paused",
    "customer.subscription.resumed",
    "invoice.paid",
    "invoice.payment_failed",
]

PLANS = [
    # (plan, product name, flat centavos, metered BRL-centavos por centavo de GMV)
    ("start", "LiveCart Start", 14700, "0.018"),
    ("grow", "LiveCart Grow", 29700, "0.013"),
    ("scale", "LiveCart Scale", 69700, "0.01"),
]


def req(method, path, form=None):
    data = urllib.parse.urlencode(form, doseq=True).encode() if form else None
    r = urllib.request.Request(f"{API}{path}", data=data, method=method)
    r.add_header("Authorization", f"Bearer {KEY}")
    if data:
        r.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        with urllib.request.urlopen(r) as resp:
            return json.load(resp)
    except urllib.error.HTTPError as e:
        body = e.read().decode()
        sys.exit(f"ERRO {e.code} em {method} {path}:\n{body}")


def main():
    if not KEY.startswith("sk_live_"):
        sys.exit("STRIPE_LIVE_SECRET_KEY ausente ou nao e sk_live_ — abortando.")

    acct = req("GET", "/account")
    if not acct.get("charges_enabled"):
        sys.exit(
            f"Conta {acct['id']} ainda nao tem charges_enabled=true — "
            "complete o onboarding live no dashboard antes de provisionar."
        )
    print(f"Conta live OK: {acct['id']} ({acct.get('business_profile', {}).get('name')})")

    # 1. Meter gmv_cents
    meters = req("GET", "/billing/meters?limit=100")["data"]
    meter = next((m for m in meters if m["event_name"] == "gmv_cents" and m["status"] == "active"), None)
    if meter:
        print(f"Meter gmv_cents ja existe: {meter['id']}")
    else:
        meter = req("POST", "/billing/meters", {
            "display_name": "GMV pago (centavos)",
            "event_name": "gmv_cents",
            "default_aggregation[formula]": "sum",
            "customer_mapping[event_payload_key]": "stripe_customer_id",
            "customer_mapping[type]": "by_id",
            "value_settings[event_payload_key]": "value",
        })
        print(f"Meter criado: {meter['id']}")

    # 2. Products + prices
    products = req("GET", "/products?limit=100&active=true")["data"]
    prices = req("GET", "/prices?limit=100&active=true")["data"]
    env = {"STRIPE_GMV_METER_EVENT": "gmv_cents"}

    for plan, name, flat_cents, gmv_rate in PLANS:
        prod = next((p for p in products if p.get("metadata", {}).get("plan") == plan), None)
        if prod:
            print(f"Product {plan} ja existe: {prod['id']}")
        else:
            prod = req("POST", "/products", {"name": name, "metadata[plan]": plan})
            print(f"Product criado: {prod['id']} ({name})")

        flat_nick, gmv_nick = f"{plan}-flat", f"{plan}-gmv"

        flat = next((p for p in prices if p.get("nickname") == flat_nick), None)
        if not flat:
            flat = req("POST", "/prices", {
                "product": prod["id"],
                "nickname": flat_nick,
                "currency": "brl",
                "unit_amount": str(flat_cents),
                "recurring[interval]": "month",
            })
            print(f"Price criado: {flat['id']} ({flat_nick} R${flat_cents / 100:.2f})")
        else:
            print(f"Price {flat_nick} ja existe: {flat['id']}")

        gmv = next((p for p in prices if p.get("nickname") == gmv_nick), None)
        if not gmv:
            gmv = req("POST", "/prices", {
                "product": prod["id"],
                "nickname": gmv_nick,
                "currency": "brl",
                "unit_amount_decimal": gmv_rate,
                "recurring[interval]": "month",
                "recurring[usage_type]": "metered",
                "recurring[meter]": meter["id"],
            })
            print(f"Price criado: {gmv['id']} ({gmv_nick} {gmv_rate} centavo/centavo GMV)")
        else:
            print(f"Price {gmv_nick} ja existe: {gmv['id']}")

        env[f"STRIPE_PRICE_{plan.upper()}_FLAT"] = flat["id"]
        env[f"STRIPE_PRICE_{plan.upper()}_METERED"] = gmv["id"]

    # 3. Webhook endpoint de producao
    hooks = req("GET", "/webhook_endpoints?limit=100")["data"]
    hook = next((w for w in hooks if w["url"] == WEBHOOK_URL), None)
    if hook:
        print(f"Webhook ja existe: {hook['id']} — pegue o whsec_ no dashboard")
        env["STRIPE_WEBHOOK_SECRET"] = "<ja existente — copiar do dashboard>"
    else:
        form = {"url": WEBHOOK_URL, "description": "LiveCart billing — producao (Railway)"}
        form["enabled_events[]"] = WEBHOOK_EVENTS
        hook = req("POST", "/webhook_endpoints", form)
        print(f"Webhook criado: {hook['id']}")
        env["STRIPE_WEBHOOK_SECRET"] = hook["secret"]

    env["STRIPE_SECRET_KEY"] = KEY

    print("\n===== VARIAVEIS PARA O RAILWAY (production / livecart-be) =====")
    for k in sorted(env):
        print(f"{k}={env[k]}")


if __name__ == "__main__":
    main()
