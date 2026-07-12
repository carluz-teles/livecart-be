# Verificações SQL por fase

Rodar via `$PSQL` (Fase 0). Substituir os placeholders `<...>`. Toda fase de escrita deve ser seguida da sua verificação — não avançar com estado inconsistente.

## § Fase 0 — pré-flight

```sql
-- Identidade dev (clerk_id com membership ativa)
SELECT u.clerk_id, u.email, m.store_id, s.name
FROM memberships m JOIN users u ON u.id = m.user_id JOIN stores s ON s.id = m.store_id
WHERE m.status = 'active' LIMIT 5;

-- Integrações da loja (decide o modo de pagamento e se frete cota)
SELECT type, provider, status FROM integrations
WHERE store_id = '<STORE_ID>' AND type IN ('payment','shipping');

-- CEP de origem da loja (sem ele a cotação retorna 422 "loja sem CEP de origem cadastrado")
SELECT id, name, COALESCE(address_zip, '') AS origem FROM stores WHERE id = '<STORE_ID>';

-- Keyword disponível?
SELECT id, name FROM products WHERE store_id = '<STORE_ID>' AND keyword = '<KEYWORD>';
-- deve retornar vazio antes de criar o produto
```

## § Fase 2 — produto criado

```sql
SELECT id, name, keyword, price, stock, weight_grams, height_cm, width_cm, length_cm
FROM products WHERE store_id = '<STORE_ID>' AND name LIKE '[E2E]%' ORDER BY created_at DESC;
```
Esperado: keyword em maiúsculas/como enviada, `stock` = valor inicial, dimensões preenchidas. **Anotar o stock inicial** — é a base dos invariantes.

## § Fase 3 — evento criado

```sql
SELECT id, title, status, type, platform_live_id, cart_expiration_minutes
FROM live_events WHERE id = '<EVENT_ID>';
-- status = 'active'

SELECT ls.id, ls.status, lsp.platform, lsp.platform_live_id
FROM live_sessions ls JOIN live_session_platforms lsp ON lsp.session_id = ls.id
WHERE ls.event_id = '<EVENT_ID>';
-- sessão 'active' com o MEDIA_ID simulado
```

## § Fase 4 — comentário → carrinho

```sql
-- Comentário registrado com o resultado esperado
SELECT comment_id, username, text, result, created_at
FROM live_comments WHERE event_id = '<EVENT_ID>' ORDER BY created_at DESC LIMIT 10;
-- results possíveis: added_to_cart, waitlisted, no_intent, no_product, blocked, paused

-- Cart criado (um por comprador por evento)
SELECT id, platform_handle, token, status, payment_status, expires_at, created_at
FROM carts WHERE event_id = '<EVENT_ID>' ORDER BY created_at;
-- status='active', payment_status='pending', expires_at ≈ now() + cart_expiration_minutes

-- Itens com preço e quantidade corretos
SELECT ci.id, p.keyword, ci.quantity, ci.unit_price, ci.waitlisted_quantity
FROM cart_items ci JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = '<CART_ID>';

-- Reserva ativa espelhando o item
SELECT product_id, quantity, status FROM stock_reservations
WHERE cart_id = '<CART_ID>';

-- Estoque decrementado
SELECT stock FROM products WHERE id = '<PRODUCT_ID>';

-- Contadores do evento (a UI/pulse lê daqui)
SELECT total_orders FROM live_events WHERE id = '<EVENT_ID>';
SELECT total_comments FROM live_sessions WHERE event_id = '<EVENT_ID>';
```

Idempotência: reenviar o MESMO comment_id não pode criar novo cart/comentário:
```sql
SELECT count(*) FROM live_comments WHERE comment_id = '<COMMENT_ID>';  -- = 1
```

## § Fase 6 — pagamento

Caminho real (cartão sandbox aprovado ou PIX confirmado no provider):
```sql
SELECT status, payment_status, payment_method, paid_at, tracking_token, short_id
FROM carts WHERE id = '<CART_ID>';
-- payment_status='paid', paid_at preenchido, tracking_token gerado (OnCartPaid)

SELECT provider, method, status, amount_cents, paid_at FROM payments
WHERE cart_id = '<CART_ID>' ORDER BY created_at DESC;

SELECT event_type, occurred_at FROM order_events WHERE cart_id = '<CART_ID>';
-- 'payment_confirmed' presente (UNIQUE(cart_id, event_type) garante exactly-once)

SELECT notification_type, channel, status FROM notification_logs
WHERE cart_id = '<CART_ID>' ORDER BY created_at DESC;
```

Modo simulado (UPDATE direto): conferir apenas `carts` + o `GET /:token/status` respondendo `paid` — e **registrar no relatório** que `payments`, `order_events`, `tracking_token` e e-mail não foram exercitados.

## § Fase 7 — waitlist

```sql
-- Item parcialmente atendido
SELECT ci.cart_id, ci.quantity, ci.waitlisted_quantity FROM cart_items ci
WHERE ci.cart_id = '<CART_B>';

-- Fila
SELECT id, cart_id, quantity, position, status, notified_at, expires_at
FROM waitlist_items WHERE event_id = '<EVENT_ID>' ORDER BY position;
-- antes da liberação: status='waiting'; depois: 'notified' + expires_at estendido

-- Cart B ganhou prazo extra na promoção
SELECT expires_at FROM carts WHERE id = '<CART_B>';
```

## § Fase 8 — expiração

Antes da varredura (após o UPDATE de `expires_at`):
```sql
SELECT status, expires_at FROM carts WHERE id = '<CART_A>';
-- status AINDA 'active' (lazy) — e o GET público ainda responde 200
```

Depois do novo comentário no mesmo produto:
```sql
SELECT status FROM carts WHERE id = '<CART_A>';                      -- 'expired'
SELECT status, reversed_at FROM stock_reservations
WHERE cart_id = '<CART_A>';                                          -- 'reversed'
SELECT stock FROM products WHERE id = '<PRODUCT_ID>';                -- devolvido
```

## § Fase 9 — fim do evento

```sql
SELECT status FROM live_events WHERE id = '<EVENT_ID>';              -- 'ended'
SELECT status, count(*) FROM carts WHERE event_id = '<EVENT_ID>' GROUP BY status;
-- 'active' → 'checkout' (pagos/expirados não mudam)
```

## § Invariantes (rodar ao final de cada cenário)

```sql
-- 1. Estoque nunca negativo
SELECT id, name, stock FROM products WHERE store_id = '<STORE_ID>' AND stock < 0;
-- deve retornar vazio

-- 2. Conservação de estoque do produto de teste:
--    stock_atual + reservas ativas + qty de carts pagos (reservas convertidas) = stock_inicial
SELECT
  (SELECT stock FROM products WHERE id = '<PRODUCT_ID>') AS stock_atual,
  (SELECT COALESCE(SUM(quantity),0) FROM stock_reservations
    WHERE product_id = '<PRODUCT_ID>' AND status = 'active') AS reservado,
  (SELECT COALESCE(SUM(quantity),0) FROM stock_reservations
    WHERE product_id = '<PRODUCT_ID>' AND status = 'converted') AS convertido;
-- stock_atual + reservado + convertido = stock inicial anotado na Fase 2

-- 3. Todo cart ativo/checkout não-pago tem reserva ativa para cada item não-waitlisted
SELECT c.id FROM carts c JOIN cart_items ci ON ci.cart_id = c.id
LEFT JOIN stock_reservations sr
  ON sr.cart_id = c.id AND sr.product_id = ci.product_id AND sr.status = 'active'
WHERE c.event_id = '<EVENT_ID>' AND c.status IN ('active','checkout')
  AND c.payment_status <> 'paid' AND ci.quantity > ci.waitlisted_quantity
  AND sr.id IS NULL;
-- deve retornar vazio

-- 4. Nenhum cart expirado segurando reserva ativa
SELECT c.id FROM carts c JOIN stock_reservations sr ON sr.cart_id = c.id
WHERE c.event_id = '<EVENT_ID>' AND c.status = 'expired' AND sr.status = 'active';
-- deve retornar vazio

-- 5. Comentários processados exatamente uma vez
SELECT comment_id, count(*) FROM live_comments
WHERE event_id = '<EVENT_ID>' GROUP BY comment_id HAVING count(*) > 1;
-- deve retornar vazio
```

## Utilitários

```sql
-- Forçar expiração de um cart
UPDATE carts SET expires_at = now() - interval '1 minute' WHERE id = '<CART_ID>';

-- Simular pagamento (Modo B — declarar limitações no relatório)
UPDATE carts SET payment_status='paid', paid_at=now(), payment_method='pix', status='checkout'
WHERE id = '<CART_ID>';

-- Cleanup da rodada (com confirmação do usuário; live_events cascateia)
DELETE FROM live_events WHERE id = '<EVENT_ID>';
DELETE FROM products WHERE id = '<PRODUCT_ID>' AND name LIKE '[E2E]%';
```
