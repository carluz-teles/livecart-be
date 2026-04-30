-- Demo seed for the Orders screen on store "Teste Loja Mercadopago"
-- (c58ddaec-8567-43d9-85b4-68b72bf1e1de). Idempotent — safe to re-run.
--
-- Existing in this store:
--   products: Camiseta Preta (PRET, R$79,90), Calça Jeans (JEAN, R$149,90),
--             Tênis Branco (TENI, R$199,90)
--   live event: Live de sabado (active)
--
-- This script adds:
--   - 3 named live events covering different ages (7d, 3d, today)
--   - 1 live session + Instagram platform record per event
--   - 6 customers
--   - 10 carts/orders covering every state combination we render
--   - cart_items, live_comments, 1 shipment with tracking events

BEGIN;

-- ----------------------------------------------------------------------------
-- Events
-- ----------------------------------------------------------------------------
INSERT INTO live_events (id, store_id, title, status, type, created_at, updated_at) VALUES
  ('a1111111-1111-1111-1111-111111111111', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'Live de Quinta — Coleção Outono', 'ended',  'single', NOW() - INTERVAL '7 days', NOW() - INTERVAL '7 days'),
  ('a2222222-2222-2222-2222-222222222222', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'Live de Sábado — Liquidação',     'ended',  'single', NOW() - INTERVAL '3 days', NOW() - INTERVAL '3 days'),
  ('a3333333-3333-3333-3333-333333333333', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'Live de Hoje — Lançamento',       'active', 'single', NOW() - INTERVAL '2 hours', NOW())
ON CONFLICT (id) DO NOTHING;

-- ----------------------------------------------------------------------------
-- Sessions
-- ----------------------------------------------------------------------------
INSERT INTO live_sessions (id, event_id, status, started_at, ended_at, total_comments) VALUES
  ('b1111111-1111-1111-1111-111111111111', 'a1111111-1111-1111-1111-111111111111', 'ended',  NOW() - INTERVAL '7 days', NOW() - INTERVAL '7 days' + INTERVAL '90 minutes', 142),
  ('b2222222-2222-2222-2222-222222222222', 'a2222222-2222-2222-2222-222222222222', 'ended',  NOW() - INTERVAL '3 days', NOW() - INTERVAL '3 days' + INTERVAL '2 hours', 311),
  ('b3333333-3333-3333-3333-333333333333', 'a3333333-3333-3333-3333-333333333333', 'active', NOW() - INTERVAL '2 hours', NULL, 47)
ON CONFLICT (id) DO NOTHING;

INSERT INTO live_session_platforms (id, session_id, platform, platform_live_id, added_at) VALUES
  ('c1111111-1111-1111-1111-111111111111', 'b1111111-1111-1111-1111-111111111111', 'instagram', 'demo_ig_live_001', NOW() - INTERVAL '7 days'),
  ('c2222222-2222-2222-2222-222222222222', 'b2222222-2222-2222-2222-222222222222', 'instagram', 'demo_ig_live_002', NOW() - INTERVAL '3 days'),
  ('c3333333-3333-3333-3333-333333333333', 'b3333333-3333-3333-3333-333333333333', 'instagram', 'demo_ig_live_003', NOW() - INTERVAL '2 hours')
ON CONFLICT (platform_live_id) DO NOTHING;

-- ----------------------------------------------------------------------------
-- Customers
-- ----------------------------------------------------------------------------
INSERT INTO customers (id, store_id, platform_user_id, platform_handle, email, phone, first_order_at, last_order_at) VALUES
  ('d1111111-1111-1111-1111-111111111111', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'ig_user_001', 'mariana.costa',     'mariana.costa@example.com',  '11987654321', NOW() - INTERVAL '60 days', NOW() - INTERVAL '7 days'),
  ('d2222222-2222-2222-2222-222222222222', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'ig_user_002', 'pedro.silva',        'pedro.silva@example.com',    '11912345678', NOW() - INTERVAL '90 days', NOW() - INTERVAL '3 days'),
  ('d3333333-3333-3333-3333-333333333333', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'ig_user_003', 'juliana.reis',       'ju.reis@example.com',        '11955544433', NOW() - INTERVAL '3 days', NOW() - INTERVAL '3 days'),
  ('d4444444-4444-4444-4444-444444444444', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'ig_user_004', 'rafa.almeida',       NULL,                         NULL,          NULL, NULL),
  ('d5555555-5555-5555-5555-555555555555', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'ig_user_005', 'camila.fashion',     'camila@example.com',         '21998877665', NOW() - INTERVAL '2 hours', NOW() - INTERVAL '2 hours'),
  ('d6666666-6666-6666-6666-666666666666', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'ig_user_006', 'lucas.tk',           NULL,                         NULL,          NULL, NULL),
  ('d7777777-7777-7777-7777-777777777777', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'ig_user_007', 'bruno.lima',         'bruno.lima@example.com',     '11933221100', NOW() - INTERVAL '2 days', NOW() - INTERVAL '2 days'),
  ('d8888888-8888-8888-8888-888888888888', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'ig_user_008', 'aline.souza',        'aline.souza@example.com',    '11944556677', NOW() - INTERVAL '6 days', NOW() - INTERVAL '6 days')
ON CONFLICT (store_id, platform_user_id) DO NOTHING;

-- ----------------------------------------------------------------------------
-- Carts (orders) — 10 carts spanning every visible state
-- ----------------------------------------------------------------------------
INSERT INTO carts (
  id, event_id, session_id, customer_id, platform_user_id, platform_handle,
  token, status, payment_status, paid_at, created_at, expires_at,
  customer_name, customer_email, customer_document, customer_phone,
  shipping_address,
  shipping_service_id, shipping_service_name, shipping_carrier,
  shipping_cost_cents, shipping_cost_real_cents, shipping_deadline_days, shipping_quoted_at,
  payment_method
) VALUES
  -- 1. DELIVERED — ticket alto, com shipment + tracking events
  ('e0000001-0000-0000-0000-000000000001',
   'a1111111-1111-1111-1111-111111111111', 'b1111111-1111-1111-1111-111111111111',
   'd1111111-1111-1111-1111-111111111111', 'ig_user_001', 'mariana.costa',
   'tok_demo_001', 'completed', 'paid',
   NOW() - INTERVAL '7 days' + INTERVAL '40 minutes',
   NOW() - INTERVAL '7 days', NULL,
   'Mariana Costa', 'mariana.costa@example.com', '12345678900', '11987654321',
   '{"zipCode":"01310100","street":"Avenida Paulista","number":"1578","complement":"apto 82","neighborhood":"Bela Vista","city":"São Paulo","state":"SP"}'::jsonb,
   'me_pac_001', 'PAC',         'Correios',     2490, 2490, 7, NOW() - INTERVAL '7 days', 'pix'),

  -- 2. PAID + shipment created, em trânsito
  ('e0000002-0000-0000-0000-000000000002',
   'a2222222-2222-2222-2222-222222222222', 'b2222222-2222-2222-2222-222222222222',
   'd2222222-2222-2222-2222-222222222222', 'ig_user_002', 'pedro.silva',
   'tok_demo_002', 'completed', 'paid',
   NOW() - INTERVAL '3 days' + INTERVAL '15 minutes',
   NOW() - INTERVAL '3 days', NULL,
   'Pedro Silva', 'pedro.silva@example.com', '98765432100', '11912345678',
   '{"zipCode":"04538133","street":"Rua Funchal","number":"410","complement":"","neighborhood":"Vila Olímpia","city":"São Paulo","state":"SP"}'::jsonb,
   'me_sedex_001', 'SEDEX',     'Correios',     3990, 3990, 3, NOW() - INTERVAL '3 days', 'card'),

  -- 3. PAID mas sem shipment ainda (tem 5h — flagado pelos insights)
  ('e0000003-0000-0000-0000-000000000003',
   'a2222222-2222-2222-2222-222222222222', 'b2222222-2222-2222-2222-222222222222',
   'd3333333-3333-3333-3333-333333333333', 'ig_user_003', 'juliana.reis',
   'tok_demo_003', 'checkout', 'paid',
   NOW() - INTERVAL '5 hours',
   NOW() - INTERVAL '3 days', NULL,
   'Juliana Reis', 'ju.reis@example.com', '11122233344', '11955544433',
   '{"zipCode":"22070001","street":"Rua Visconde de Pirajá","number":"550","complement":"sala 1201","neighborhood":"Ipanema","city":"Rio de Janeiro","state":"RJ"}'::jsonb,
   'me_pac_001', 'PAC',         'Correios',     2890, 2890, 5, NOW() - INTERVAL '6 hours', 'pix'),

  -- 4. CHECKOUT + customer info preenchido, aguardando pagamento
  ('e0000004-0000-0000-0000-000000000004',
   'a3333333-3333-3333-3333-333333333333', 'b3333333-3333-3333-3333-333333333333',
   'd5555555-5555-5555-5555-555555555555', 'ig_user_005', 'camila.fashion',
   'tok_demo_004', 'checkout', 'pending', NULL,
   NOW() - INTERVAL '90 minutes', NOW() + INTERVAL '22 hours',
   'Camila Souza', 'camila@example.com', '55566677788', '21998877665',
   '{"zipCode":"20040020","street":"Avenida Rio Branco","number":"185","complement":"15º andar","neighborhood":"Centro","city":"Rio de Janeiro","state":"RJ"}'::jsonb,
   'smart_001', 'Expressa',     'SmartEnvios', 1990, 1990, 4, NOW() - INTERVAL '60 minutes', NULL),

  -- 5. CHECKOUT sem dados ainda (cliente abriu mas não preencheu)
  ('e0000005-0000-0000-0000-000000000005',
   'a3333333-3333-3333-3333-333333333333', 'b3333333-3333-3333-3333-333333333333',
   'd4444444-4444-4444-4444-444444444444', 'ig_user_004', 'rafa.almeida',
   'tok_demo_005', 'checkout', 'pending', NULL,
   NOW() - INTERVAL '40 minutes', NOW() + INTERVAL '23 hours',
   '', '', '', '',
   '{}'::jsonb,
   '', '', '', 0, 0, 0, NULL, NULL),

  -- 6. ACTIVE — cart vivo durante a live de hoje
  ('e0000006-0000-0000-0000-000000000006',
   'a3333333-3333-3333-3333-333333333333', 'b3333333-3333-3333-3333-333333333333',
   'd6666666-6666-6666-6666-666666666666', 'ig_user_006', 'lucas.tk',
   'tok_demo_006', 'active', 'unpaid', NULL,
   NOW() - INTERVAL '15 minutes', NOW() + INTERVAL '24 hours',
   '', '', '', '',
   '{}'::jsonb,
   '', '', '', 0, 0, 0, NULL, NULL),

  -- 7. EXPIRED
  ('e0000007-0000-0000-0000-000000000007',
   'a1111111-1111-1111-1111-111111111111', 'b1111111-1111-1111-1111-111111111111',
   NULL, 'ig_user_899', 'expired_customer',
   'tok_demo_007', 'expired', 'pending', NULL,
   NOW() - INTERVAL '6 days', NOW() - INTERVAL '5 days',
   '', '', '', '',
   '{}'::jsonb,
   '', '', '', 0, 0, 0, NULL, NULL),

  -- 8. FAILED — pagamento recusado
  ('e0000008-0000-0000-0000-000000000008',
   'a2222222-2222-2222-2222-222222222222', 'b2222222-2222-2222-2222-222222222222',
   'd7777777-7777-7777-7777-777777777777', 'ig_user_007', 'bruno.lima',
   'tok_demo_008', 'checkout', 'failed', NULL,
   NOW() - INTERVAL '2 days' - INTERVAL '20 minutes', NOW() + INTERVAL '12 hours',
   'Bruno Lima', 'bruno.lima@example.com', '22233344455', '11933221100',
   '{"zipCode":"05407002","street":"Rua dos Pinheiros","number":"800","complement":"","neighborhood":"Pinheiros","city":"São Paulo","state":"SP"}'::jsonb,
   'me_pac_001', 'PAC',         'Correios',     2490, 2490, 7, NOW() - INTERVAL '2 days', 'card'),

  -- 9. REFUNDED — completo, mas reembolsado
  ('e0000009-0000-0000-0000-000000000009',
   'a1111111-1111-1111-1111-111111111111', 'b1111111-1111-1111-1111-111111111111',
   'd8888888-8888-8888-8888-888888888888', 'ig_user_008', 'aline.souza',
   'tok_demo_009', 'completed', 'refunded',
   NOW() - INTERVAL '6 days' + INTERVAL '30 minutes',
   NOW() - INTERVAL '6 days', NULL,
   'Aline Souza', 'aline.souza@example.com', '33344455566', '11944556677',
   '{"zipCode":"30130180","street":"Avenida Afonso Pena","number":"1500","complement":"","neighborhood":"Centro","city":"Belo Horizonte","state":"MG"}'::jsonb,
   'me_pac_001', 'PAC',         'Correios',     2490, 2490, 7, NOW() - INTERVAL '6 days', 'pix'),

  -- 10. CHECKOUT/PAID alto valor — pra ver insight de "+X% vs ticket médio"
  ('e0000010-0000-0000-0000-000000000010',
   'a2222222-2222-2222-2222-222222222222', 'b2222222-2222-2222-2222-222222222222',
   'd5555555-5555-5555-5555-555555555555', 'ig_user_005', 'camila.fashion',
   'tok_demo_010', 'checkout', 'paid',
   NOW() - INTERVAL '20 minutes',
   NOW() - INTERVAL '3 days', NULL,
   'Camila Souza', 'camila@example.com', '55566677788', '21998877665',
   '{"zipCode":"20040020","street":"Avenida Rio Branco","number":"185","complement":"15º andar","neighborhood":"Centro","city":"Rio de Janeiro","state":"RJ"}'::jsonb,
   'me_sedex_001', 'SEDEX',     'Correios',     3990, 3990, 3, NOW() - INTERVAL '3 days', 'card')
ON CONFLICT (id) DO NOTHING;

-- ----------------------------------------------------------------------------
-- Cart Items (use existing 3 products)
-- ----------------------------------------------------------------------------
INSERT INTO cart_items (id, cart_id, product_id, quantity, unit_price) VALUES
  -- order 1: 1 camiseta + 1 tênis
  ('f0000001-0001-0000-0000-000000000001', 'e0000001-0000-0000-0000-000000000001', '6495ba54-26ac-4dca-bb75-7613a479118e', 1, 7990),
  ('f0000001-0002-0000-0000-000000000001', 'e0000001-0000-0000-0000-000000000001', 'fd47f706-002b-41e4-9aa4-87b43cdeb62b', 1, 19990),

  -- order 2: 2 camisetas
  ('f0000002-0001-0000-0000-000000000002', 'e0000002-0000-0000-0000-000000000002', '6495ba54-26ac-4dca-bb75-7613a479118e', 2, 7990),

  -- order 3: 1 calça + 1 tênis
  ('f0000003-0001-0000-0000-000000000003', 'e0000003-0000-0000-0000-000000000003', '463c0166-54b8-4956-a92e-77228c078e67', 1, 14990),
  ('f0000003-0002-0000-0000-000000000003', 'e0000003-0000-0000-0000-000000000003', 'fd47f706-002b-41e4-9aa4-87b43cdeb62b', 1, 19990),

  -- order 4: 3 calças
  ('f0000004-0001-0000-0000-000000000004', 'e0000004-0000-0000-0000-000000000004', '463c0166-54b8-4956-a92e-77228c078e67', 3, 14990),

  -- order 5: 1 camiseta
  ('f0000005-0001-0000-0000-000000000005', 'e0000005-0000-0000-0000-000000000005', '6495ba54-26ac-4dca-bb75-7613a479118e', 1, 7990),

  -- order 6: 1 calça (live ativa)
  ('f0000006-0001-0000-0000-000000000006', 'e0000006-0000-0000-0000-000000000006', '463c0166-54b8-4956-a92e-77228c078e67', 1, 14990),

  -- order 7 (expired): 1 camiseta
  ('f0000007-0001-0000-0000-000000000007', 'e0000007-0000-0000-0000-000000000007', '6495ba54-26ac-4dca-bb75-7613a479118e', 1, 7990),

  -- order 8 (failed): 1 tênis
  ('f0000008-0001-0000-0000-000000000008', 'e0000008-0000-0000-0000-000000000008', 'fd47f706-002b-41e4-9aa4-87b43cdeb62b', 1, 19990),

  -- order 9 (refunded): 1 calça
  ('f0000009-0001-0000-0000-000000000009', 'e0000009-0000-0000-0000-000000000009', '463c0166-54b8-4956-a92e-77228c078e67', 1, 14990),

  -- order 10 (high ticket): 5 tênis (R$999,50 + frete)
  ('f0000010-0001-0000-0000-000000000010', 'e0000010-0000-0000-0000-000000000010', 'fd47f706-002b-41e4-9aa4-87b43cdeb62b', 5, 19990)
ON CONFLICT (cart_id, product_id) DO NOTHING;

-- ----------------------------------------------------------------------------
-- Live comments — coluna de "Live de origem" precisa de comentários reais
-- ----------------------------------------------------------------------------
INSERT INTO live_comments (
  id, session_id, event_id, platform, platform_comment_id, platform_user_id,
  platform_handle, text, has_purchase_intent, matched_product_id, matched_quantity, result, created_at
) VALUES
  -- Mariana, Order 1, Live de Quinta
  ('a0c00001-0001-0000-0000-000000000001', 'b1111111-1111-1111-1111-111111111111', 'a1111111-1111-1111-1111-111111111111', 'instagram', 'cmt_001_a', 'ig_user_001', 'mariana.costa', 'amei a camiseta!', false, NULL, NULL, 'no_match', NOW() - INTERVAL '7 days' + INTERVAL '5 minutes'),
  ('a0c00001-0002-0000-0000-000000000001', 'b1111111-1111-1111-1111-111111111111', 'a1111111-1111-1111-1111-111111111111', 'instagram', 'cmt_001_b', 'ig_user_001', 'mariana.costa', 'PRET',          true,  '6495ba54-26ac-4dca-bb75-7613a479118e', 1, 'matched', NOW() - INTERVAL '7 days' + INTERVAL '12 minutes'),
  ('a0c00001-0003-0000-0000-000000000001', 'b1111111-1111-1111-1111-111111111111', 'a1111111-1111-1111-1111-111111111111', 'instagram', 'cmt_001_c', 'ig_user_001', 'mariana.costa', 'TENI',          true,  'fd47f706-002b-41e4-9aa4-87b43cdeb62b', 1, 'matched', NOW() - INTERVAL '7 days' + INTERVAL '23 minutes'),

  -- Pedro, Order 2
  ('a0c00002-0001-0000-0000-000000000002', 'b2222222-2222-2222-2222-222222222222', 'a2222222-2222-2222-2222-222222222222', 'instagram', 'cmt_002_a', 'ig_user_002', 'pedro.silva',  'tem essa camiseta em outras cores?', false, NULL, NULL, 'no_match', NOW() - INTERVAL '3 days' + INTERVAL '3 minutes'),
  ('a0c00002-0002-0000-0000-000000000002', 'b2222222-2222-2222-2222-222222222222', 'a2222222-2222-2222-2222-222222222222', 'instagram', 'cmt_002_b', 'ig_user_002', 'pedro.silva',  'PRET 2',                          true,  '6495ba54-26ac-4dca-bb75-7613a479118e', 2, 'matched', NOW() - INTERVAL '3 days' + INTERVAL '7 minutes'),

  -- Juliana, Order 3
  ('a0c00003-0001-0000-0000-000000000003', 'b2222222-2222-2222-2222-222222222222', 'a2222222-2222-2222-2222-222222222222', 'instagram', 'cmt_003_a', 'ig_user_003', 'juliana.reis', 'JEAN',          true, '463c0166-54b8-4956-a92e-77228c078e67', 1, 'matched', NOW() - INTERVAL '3 days' + INTERVAL '8 minutes'),
  ('a0c00003-0002-0000-0000-000000000003', 'b2222222-2222-2222-2222-222222222222', 'a2222222-2222-2222-2222-222222222222', 'instagram', 'cmt_003_b', 'ig_user_003', 'juliana.reis', 'TENI',          true, 'fd47f706-002b-41e4-9aa4-87b43cdeb62b', 1, 'matched', NOW() - INTERVAL '3 days' + INTERVAL '11 minutes'),

  -- Camila, Order 4 (live de hoje)
  ('a0c00004-0001-0000-0000-000000000004', 'b3333333-3333-3333-3333-333333333333', 'a3333333-3333-3333-3333-333333333333', 'instagram', 'cmt_004_a', 'ig_user_005', 'camila.fashion', 'JEAN 3', true, '463c0166-54b8-4956-a92e-77228c078e67', 3, 'matched', NOW() - INTERVAL '95 minutes'),

  -- Lucas, Order 6 (active live)
  ('a0c00006-0001-0000-0000-000000000006', 'b3333333-3333-3333-3333-333333333333', 'a3333333-3333-3333-3333-333333333333', 'instagram', 'cmt_006_a', 'ig_user_006', 'lucas.tk', 'JEAN', true, '463c0166-54b8-4956-a92e-77228c078e67', 1, 'matched', NOW() - INTERVAL '15 minutes'),

  -- Camila, Order 10 (high ticket)
  ('a0c00010-0001-0000-0000-000000000010', 'b2222222-2222-2222-2222-222222222222', 'a2222222-2222-2222-2222-222222222222', 'instagram', 'cmt_010_a', 'ig_user_005', 'camila.fashion', 'TENI 5', true, 'fd47f706-002b-41e4-9aa4-87b43cdeb62b', 5, 'matched', NOW() - INTERVAL '3 days' + INTERVAL '40 minutes')
ON CONFLICT (id) DO NOTHING;

-- ----------------------------------------------------------------------------
-- Shipment + tracking events for Order 1 (delivered) and Order 2 (in transit)
-- ----------------------------------------------------------------------------
INSERT INTO shipments (id, order_id, store_id, provider, provider_order_id, provider_order_number, tracking_code, public_tracking_url, label_url, status, status_raw_code, status_raw_name, created_at, updated_at) VALUES
  ('a5500001-0000-0000-0000-000000000001', 'e0000001-0000-0000-0000-000000000001', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'melhor_envio', 'me_order_001', 'ME-001-2026', 'NL123456789BR', 'https://www.linkcorreios.com.br/?id=NL123456789BR', 'https://example.com/labels/001.pdf', 'delivered', 7, 'Entregue', NOW() - INTERVAL '6 days', NOW() - INTERVAL '1 days'),
  ('a5500002-0000-0000-0000-000000000002', 'e0000002-0000-0000-0000-000000000002', 'c58ddaec-8567-43d9-85b4-68b72bf1e1de', 'melhor_envio', 'me_order_002', 'ME-002-2026', 'NL987654321BR', 'https://www.linkcorreios.com.br/?id=NL987654321BR', 'https://example.com/labels/002.pdf', 'in_transit', 3, 'Em trânsito', NOW() - INTERVAL '2 days', NOW() - INTERVAL '6 hours')
ON CONFLICT (id) DO NOTHING;

INSERT INTO shipment_tracking_events (id, shipment_id, status, raw_code, raw_name, observation, event_at, source) VALUES
  -- Shipment 1: created → posted → in transit → out for delivery → delivered
  ('a6600001-0001-0000-0000-000000000001', 'a5500001-0000-0000-0000-000000000001', 'pending',          1, 'Aguardando coleta',  'Etiqueta gerada', NOW() - INTERVAL '6 days' + INTERVAL '1 hour', 'webhook'),
  ('a6600001-0002-0000-0000-000000000001', 'a5500001-0000-0000-0000-000000000001', 'in_transit',       3, 'Postado',            'Postado em São Paulo / SP', NOW() - INTERVAL '5 days', 'poll'),
  ('a6600001-0003-0000-0000-000000000001', 'a5500001-0000-0000-0000-000000000001', 'in_transit',       4, 'Em trânsito',        'Centro de distribuição São Paulo / SP', NOW() - INTERVAL '4 days', 'poll'),
  ('a6600001-0004-0000-0000-000000000001', 'a5500001-0000-0000-0000-000000000001', 'out_for_delivery', 5, 'Saiu para entrega',  'Saiu para entrega ao destinatário', NOW() - INTERVAL '1 day' - INTERVAL '4 hours', 'poll'),
  ('a6600001-0005-0000-0000-000000000001', 'a5500001-0000-0000-0000-000000000001', 'delivered',        7, 'Entregue',           'Entregue ao destinatário', NOW() - INTERVAL '1 day', 'webhook'),

  -- Shipment 2: created → posted → in transit
  ('a6600002-0001-0000-0000-000000000002', 'a5500002-0000-0000-0000-000000000002', 'pending',    1, 'Aguardando coleta', 'Etiqueta gerada', NOW() - INTERVAL '2 days' + INTERVAL '1 hour', 'webhook'),
  ('a6600002-0002-0000-0000-000000000002', 'a5500002-0000-0000-0000-000000000002', 'in_transit', 3, 'Postado',           'Postado em São Paulo / SP', NOW() - INTERVAL '1 day', 'poll'),
  ('a6600002-0003-0000-0000-000000000002', 'a5500002-0000-0000-0000-000000000002', 'in_transit', 4, 'Em trânsito',       'Em rota para a unidade de destino', NOW() - INTERVAL '6 hours', 'poll')
ON CONFLICT (shipment_id, event_at, raw_code) DO NOTHING;

COMMIT;
