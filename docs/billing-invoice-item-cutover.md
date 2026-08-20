# Cutover: comissão GMV metered → InvoiceItem

Modelo novo: a mensalidade é uma subscription flat (+ cupom nativo); a comissão
(taxa) sobre GMV deixa de ser um preço *metered* na Stripe e passa a ser um
**InvoiceItem** calculado do ledger e injetado na fatura do ciclo
(`OnSubscriptionCycleInvoice`, webhook `invoice.created`).

> **Pré-requisitos (ORDEM OBRIGATÓRIA):**
> 1. Código novo **deployado**.
> 2. Fluxo validado em **Stripe TEST mode** (skill `billing-e2e`): checkout
>    subscription-mode → `invoice.created` do ciclo gera o InvoiceItem da taxa →
>    refund neta no ciclo seguinte → promo de taxa desconta.
> 3. `invoice.created` habilitado no endpoint (JÁ FEITO em live: `we_1Ttref…`).
>
> Só então rodar os passos abaixo. Migrar as subs ANTES do deploy deixaria as 4
> lojas sem cobrança de taxa (o metered sai, o InvoiceItem ainda não roda).

Todos os comandos usam `$SK` = `STRIPE_SECRET_KEY` (live) do ambiente.

## 1. Migrar as 4 subs Grow ativas (remover o item metered)

Para cada subscription Grow ativa que ainda carrega o item metered
(`price_1Ttrec1fjFMS2YMZel0Yoxko` ou o novo `price_1U6YVe1fjFMS2YMZ5DL6DON6`):

```bash
# listar itens da sub e achar o item metered
curl -s https://api.stripe.com/v1/subscriptions/<SUB_ID> -u "$SK:" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);[print(i["id"],i["price"]["id"],i["price"].get("recurring",{}).get("usage_type")) for i in d["items"]["data"]]'

# remover o item metered SEM proration
curl -s -X DELETE "https://api.stripe.com/v1/subscription_items/<METERED_ITEM_ID>" \
  -u "$SK:" -d "proration_behavior=none"
```

## 2. "Settle" das entradas de ledger pré-cutover

Evita que uma venda antiga com `stripe_ref` NULL (ex.: meter que falhou) seja
faturada retroativamente no primeiro ciclo pós-cutover. Rodar UMA vez, no
cutover, para as lojas migradas (ou todas):

```sql
UPDATE billing_ledger_entries
SET stripe_ref = 'pre-cutover-settled'
WHERE billable = true AND stripe_ref IS NULL
  AND created_at < '<TIMESTAMP_DO_CUTOVER>';
```

A partir daí, só vendas novas (stripe_ref NULL) entram no sweep do ciclo.

## 3. Seed da promo de taxa da Canto da Arte

O cupom `CANTODAART` (`OMmiIFhd`) desconta a **mensalidade** (nativo Stripe).
O desconto da **taxa** é uma promo de domínio — criar quando a loja assinar:

```sql
-- 50% de desconto na taxa, 1 ciclo
INSERT INTO billing_taxa_promos (store_id, discount_bps, cycles_remaining, code, description)
VALUES ('<STORE_ID_CANTO_DA_ARTE>', 5000, 1, 'CANTODAART', 'Canto da Arte — 50% taxa ciclo 1');
```

(ou via `Service.CreateTaxaPromo(ctx, storeID, 5000, 1, "CANTODAART", "…")`.)

## 4. Cleanup Stripe (só APÓS as 4 subs migradas)

```bash
# arquivar os preços metered (não deleta; subs já não os referenciam)
curl -s https://api.stripe.com/v1/prices/price_1Ttrec1fjFMS2YMZel0Yoxko -u "$SK:" -d active=false
curl -s https://api.stripe.com/v1/prices/price_1U6YVe1fjFMS2YMZ5DL6DON6 -u "$SK:" -d active=false
# arquivar o product de GMV novo
curl -s https://api.stripe.com/v1/products/prod_V6m3uy8vFuCHfZ -u "$SK:" -d active=false
# apagar o cupom de GMV que não é mais usado
curl -s -X DELETE https://api.stripe.com/v1/coupons/HUsuAUSc -u "$SK:"
```

## 5. Env que ficam obsoletas (podem ser removidas)

- `STRIPE_GMV_METER_EVENT` — o meter não é mais usado.
- `STRIPE_PRICE_GROW_METERED` / `*_START_METERED` / `*_SCALE_METERED` — só ainda
  usadas pelo caminho legado setup-mode (`ActivateSubscription`) enquanto a
  coorte de trials antigos drena; remover quando não houver mais sub legada.
