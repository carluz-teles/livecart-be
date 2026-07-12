---
name: live-flow-e2e
description: Testa o fluxo de live commerce de ponta a ponta pela UI real com Playwright (local ou staging) — loga na plataforma via Clerk, cria produto e evento, simula comentários via webhook do Instagram, confere no painel do evento que os comentários chegaram e os carrinhos foram gerados, preenche o checkout do comprador e paga, verificando estoque, waitlist, expiração e banco a cada fase. Usar quando pedirem para testar/simular/validar o fluxo da live, rodar o E2E, simular comentários ou compradores, testar expiração/estoque/pagamento, ou conferir o pipeline webhook→carrinho→checkout→pagamento na interface.
---

# Live Flow E2E — teste do fluxo completo pela UI com Playwright

Playbook para dirigir o fluxo real do sistema de ponta a ponta: login no painel via Clerk, evento recebendo comentários simulados (webhook), verificação **na UI do evento** de que comentários e carrinhos aparecem, checkout do comprador preenchido e pago pela UI, com o estado do banco conferido a cada fase.

Usar as tools do Playwright MCP (`browser_navigate`, `browser_snapshot`, `browser_fill_form`, `browser_click`, `browser_wait_for`, `browser_take_screenshot`) para tudo que é UI. O que não tem UI (webhook de comentário) vai via API; o que a UI não expõe (reservas de estoque, waitlist) confere-se via SQL.

Arquivos de apoio (ler conforme a fase exigir):
- `references/flow-map.md` — mapa técnico BE + FE: endpoints, tabelas, estados, rotas e componentes.
- `references/payloads.md` — payloads exatos (webhook Instagram, checkout API), seletores da UI e dados de teste (CPF, CEP, cartões sandbox).
- `references/db-checks.md` — queries SQL de verificação por fase + invariantes de estoque.
- `scripts/send-comment.sh` — helper para disparar comentários simulados.

## Regras invioláveis

1. **Nunca rodar contra produção.** Alvos válidos: `local` e `staging`. O bypass de auth dev (`X-Dev-User-ID`) só existe quando `ENVIRONMENT != production` — um 401 nas rotas admin mesmo com o header é sinal de alvo errado: **parar imediatamente**.
2. **Backend local sempre via `docker compose up`**; frontend local sempre via `npm run dev` (regras do CLAUDE.md — nunca `go run`). Staging já está no ar (Railway); não fazer deploy como parte desta skill.
3. **`comment_id` sempre único** por webhook enviado (há guard de idempotência) — exceto quando o objetivo for testar a idempotência.
4. **Keyword sempre nova** por produto de teste: exatamente 4 caracteres alfanuméricos (ex.: `9432`), únicos por loja (`UNIQUE(store_id, keyword)`). Conferir disponibilidade no banco antes.
5. **Escopo de dados**: tudo que a simulação criar deve ser prefixado com `[E2E]` e rastreável pelo `event_id` da rodada. Cleanup ao final (Fase 10); não deletar nada que a rodada não criou. Em staging isso importa mais — o ambiente é compartilhado.
6. **Pagamento real só em sandbox/test mode.** Antes de pagar, confirmar que a integração de pagamento da loja usa credenciais de teste. Qualquer dúvida (ex.: chave live): parar e perguntar.

## Fase 0 — Ambiente, alvo e identidade

Parâmetros usados em todas as fases: `$API_BASE`, `$FE_BASE`, `$PSQL`.

**Local:**
```bash
docker compose up -d                                  # backend (raiz do repo BE)
(cd /home/carluz_teles/livecart-fe && npm run dev &)  # frontend
API_BASE="http://localhost:3001"; FE_BASE="http://localhost:3000"
PSQL() { docker compose exec -T postgres psql -U livecart -d livecart -tA -c "$1"; }
curl -s $API_BASE/health && curl -s -o /dev/null -w "%{http_code}" $FE_BASE   # aguardar ambos
```

⚠️ **Antes de subir o FE local, conferir para onde ele aponta** — o `apiClient` monta as URLs como `${NEXT_PUBLIC_API_URL}${path}`, então a env **deve incluir o sufixo `/api/v1`** e apontar para a API do alvo escolhido:
```bash
grep NEXT_PUBLIC_API_URL /home/carluz_teles/livecart-fe/.env.local
# rodada local exige: NEXT_PUBLIC_API_URL="http://localhost:3001/api/v1"
```
Se estiver apontando para produção ou outro ambiente, **parar e confirmar com o usuário** antes de alterar o `.env.local` (anotar o valor original para restaurar ao final). Sem o sufixo `/api/v1`, todo request do painel vira 404 `Cannot GET /stores/...`.

**Staging (Railway):** obter URL da API, URL do FE e `DATABASE_URL` via `railway` CLI (`railway environment`, `railway variables`) ou perguntar ao usuário — não adivinhar URLs. Confirmar com o usuário que são de staging antes de qualquer escrita.
```bash
API_BASE="https://<api-staging>"; FE_BASE="https://<fe-staging>"
PSQL() { psql "$STAGING_DATABASE_URL" -tA -c "$1"; }
```

**Duas identidades são necessárias:**
- **UI (Clerk)**: e-mail + senha de um usuário de teste para logar no painel. Ler de `/home/carluz_teles/livecart-fe/.env.local` (`E2E_CLERK_USER_USERNAME` / `E2E_CLERK_USER_PASSWORD`); se não existirem, perguntar ao usuário. Instâncias dev do Clerk aceitam e-mails `*+clerk_test@...` com código `424242`.
- **API (bypass dev)**: `users.clerk_id` de quem tem membership ativa na loja, para os passos sem UI:
```sql
SELECT u.clerk_id, u.email, m.store_id, s.name
FROM memberships m JOIN users u ON u.id = m.user_id JOIN stores s ON s.id = m.store_id
WHERE m.status = 'active' LIMIT 5;
```
Headers das rotas admin: `-H "Authorization: Bearer dev" -H "X-Dev-User-ID: <clerk_id>"` (o middleware exige o prefixo `Bearer` mesmo no bypass). Usar a **mesma loja** do usuário da UI. Em staging, loja de teste — nunca a de um merchant real.

**Pré-flight de integrações** (decide o modo de pagamento da Fase 6):
```sql
SELECT type, provider, status FROM integrations
WHERE store_id = '<STORE_ID>' AND type IN ('payment','shipping');
```
Checkout pela UI exige: integração de pagamento ativa (sandbox), integração de frete ativa, loja com CEP de origem e produto com dimensões (ver `db-checks.md § Fase 0`).

## Fase 1 — Login no painel (Clerk via Playwright)

1. `browser_navigate` → `$FE_BASE/login` (componente `<SignIn/>` do Clerk).
2. `browser_snapshot`, preencher e-mail → **"Continuar"** → senha → submeter (seletores em `payloads.md § Login`).
3. `browser_wait_for` até a URL conter `/dashboard`. Screenshot de evidência.

Se o Clerk bloquear (captcha/bot detection): usar o harness que já existe no FE (`e2e/auth.setup.ts` com `@clerk/testing`, `npx playwright test` no repo FE) para validar o login, e seguir as fases admin via UI numa sessão já autenticada — ou, em último caso, executar os passos admin via API (bypass dev) e manter no Playwright apenas checkout + verificações públicas.

## Fase 2 — Criar produto pela UI

1. `browser_navigate` → `$FE_BASE/dashboard/products` → botão **"Novo Produto"** (abre sheet).
2. Origem **Manual**; preencher: nome `[E2E] Produto Teste`, preço (centavos), estoque `10`, keyword nova de 4 caracteres, e **dimensões de frete completas** (peso/altura/largura/comprimento — regra all-or-nothing; sem elas a cotação de frete falha e o checkout trava). Seletores em `payloads.md § Produto`.
3. Salvar → aguardar toast "Produto criado com sucesso".
4. Verificar no banco (`db-checks.md § Fase 2`) e guardar `product_id`, keyword e estoque inicial (base dos invariantes).

Fallback sem UI: `POST /api/v1/stores/:storeId/products` (payload em `payloads.md`).

## Fase 3 — Criar o evento

Na UI, live de Instagram exige **selecionar uma live real** da integração (dropdown "Live Ativa") — não dá para digitar um media id simulado. Então o evento simulado é criado **via API** e monitorado pela UI:

```bash
curl -s -X POST $API_BASE/api/v1/stores/$STORE_ID/lives \
  -H "Authorization: Bearer dev" -H "X-Dev-User-ID: $CLERK_ID" -H "Content-Type: application/json" \
  -d '{"title":"[E2E] Live Teste","type":"single","platform":"instagram","platformLiveId":"SIM-'$(date +%s)'","cartExpirationMinutes":15}'
```

- Sem `scheduledAt` o evento nasce `active`, já com sessão ativa e plataforma anexada — não precisa de `/start`.
- `platformLiveId` é o `media.id` que roteará o webhook. Guardar `EVENT_ID` e `MEDIA_ID`. Usar `cartExpirationMinutes` ≥ 15 quando for pagar pela UI (o preenchimento leva tempo).

**Verificação na UI**: `browser_navigate` → `$FE_BASE/dashboard/events` → o evento `[E2E] Live Teste` aparece na listagem com status ativo; abrir a página de detalhes (`/dashboard/events/<EVENT_ID>`) e conferir o layout base (KPIs zerados, sessão listada). Verificar banco: `db-checks.md § Fase 3`.

## Fase 4 — Comentários chegando no evento (webhook + UI)

Manter a página do evento aberta no Playwright. Disparar comentários via webhook (público, sem assinatura — payload em `payloads.md`):

```bash
API_BASE=$API_BASE .claude/skills/live-flow-e2e/scripts/send-comment.sh "$MEDIA_ID" "9432" "sim_user_1" "maria_teste"
```

Processamento no BE (ordem): idempotência do comment_id → sessão/evento pelo media_id → varredura lazy de carts expirados do produto → `ParsePurchaseIntent` → match por keyword na loja (fallback: produto ativo do evento) → limite `cartMaxQuantityPerItem` → decrementa `products.stock` → cria/atualiza `carts`+`cart_items` → excedente vira `waitlist_items` → grava `live_comments` → tenta DM (**falha esperada nos logs** — não há integração Instagram; é best-effort).

**Verificação na UI do evento** (a página atualiza por polling do `/pulse` — aguardar 5–10s ou recarregar):
- Feed **Comentários**: aparece `@maria_teste` com o texto enviado e indicação de intenção de compra.
- Tabela **Pedidos**: linha nova com `@maria_teste`, status ativo, itens e valor corretos.
- **KPIs/Funil**: contadores de comentários e carrinhos incrementados.

Enviar também variações de parsing e conferir que a UI as reflete (tabela completa em `flow-map.md`): `"9432 2x"` (qty 2), `"quero 3 9432"` (qty 3), `"não quero"` / `"quanto custa"` (comentário aparece **sem** gerar carrinho), `"quero"` sem keyword (só vira cart com produto em destaque setado — testável pelo **Live Mode / Produto em Destaque** na própria UI do evento).

Verificar banco após cada comentário: `db-checks.md § Fase 4` (cart `active` com `expires_at` e `token`, reserva `active`, `products.stock` decrementado, `live_comments.result` correto).

## Fase 5 — Carrinho disponível (checkout público)

Obter o token do cart no banco (o link por DM não existe na simulação):
```sql
SELECT id, token, status, payment_status, expires_at FROM carts WHERE event_id = '<EVENT_ID>';
```

`browser_navigate` → `$FE_BASE/cart/<token>` — a página pública do comprador. Conferir: itens, quantidades, preços e resumo iguais ao que o painel mostra. Smoke opcional da edição de itens (UI ou API `POST/PATCH/DELETE /api/public/checkout/:token/items...`) conferindo que `stock_reservations` acompanha.

Nuance: cart com `expires_at` no passado mas `status='active'` ainda responde 200 — a expiração é lazy (Fase 8). A tela de "carrinho expirado" só aparece quando `status='expired'`.

## Fase 6 — Pagar o checkout pela UI (Playwright)

Wizard de 4 seções em `$FE_BASE/cart/<token>` (seletores e dados de teste em `payloads.md § Checkout UI`):

1. **Dados do comprador**: `customerName`, `customerDocument` (CPF de teste válido), `customerPhone` (opcional), `email`.
2. **Endereço**: `shippingAddress.zipCode` com CEP real — ViaCEP autopreenche rua/bairro/cidade/UF após ~400ms (cidade/UF ficam disabled; não editar). Preencher `shippingAddress.number`.
3. **Frete**: aguardar as opções (cotação dispara sozinha); clicar numa opção. Se falhar, diagnosticar nos logs da API (integração de frete? CEP de origem? dimensões?) antes de seguir.
4. **Pagamento** — escolher pelo pré-flight da Fase 0:
   - **Modo A / cartão sandbox** (fecha o loop sozinho): preencher cartão de teste (`payloads.md § Cartões`) e clicar **"Pagar R$ X,XX"**. Aprovação síncrona dispara `OnCartPaid` — a cadeia completa de pós-pagamento roda.
   - **Modo A / PIX**: clicar **"Gerar PIX"** → QR + copia-e-cola + polling de status a cada 5s. Confirmar exige simular o pagamento no sandbox do provider; sem isso valida geração + polling (fica `pending`).
   - **Modo B / sem integração de pagamento** — simular via SQL e ainda validar a UI:
     ```sql
     UPDATE carts SET payment_status='paid', paid_at=now(), payment_method='pix', status='checkout' WHERE id='<CART_ID>';
     ```
     Recarregar `$FE_BASE/cart/<token>` → deve renderizar a tela de **pedido pago**. ⚠️ Declarar no relatório o que o Modo B NÃO exercita: `postcheckout.OnCartPaid` (tracking_token, `order_events(payment_confirmed)`, e-mail, GMV), confirmação de cupom, fulfillment de waitlist, ERP e a linha em `payments`.

Webhooks de pagamento **não podem ser forjados**: `ProcessPaymentNotification` reconsulta a API do provider antes de mudar estado. Local recebe webhooks reais via `docker compose --profile dev up -d tunnel` (`https://livecart-api.loca.lt`); staging recebe direto.

**Verificação pós-pagamento na UI admin**: voltar à página do evento — cart pago na tabela de Pedidos, receita nos KPIs/funil, checkout ativo some. Banco: `db-checks.md § Fase 6`. Screenshots de evidência (checkout pago + painel).

## Fase 7 — Estoque esgotado e waitlist

1. Criar produto com estoque `1` (keyword nova — Fase 2).
2. Comentário do `user_A` → leva a unidade (reserva `active`, stock 0).
3. Comentário do `user_B` → item com `waitlisted_quantity` + linha em `waitlist_items` (`status='waiting'`, com `position`). Pedido parcial (`"XXXX 3x"` com stock 2) divide: 2 reservados + 1 na fila. A UI pública do cart de B mostra o item na fila de espera.
4. Liberar estoque: expirar o cart A (roteiro da Fase 8).
5. Verificar promoção: waitlist de B `waiting`→`notified`, `expires_at` do cart B estendido, estoque re-reservado. Saída voluntária: `DELETE /api/public/checkout/:token/waitlist/:id`.

## Fase 8 — Expiração de carrinho

Não há cron: a materialização é **lazy**, disparada dentro do processamento de um novo comentário para o mesmo produto (`ProcessExpiredCartsForProduct`):

1. Cart do `user_A` criado (Fase 4); anotar `products.stock` e reservas.
2. Forçar: `UPDATE carts SET expires_at = now() - interval '1 minute' WHERE id = '<CART_A>';`
3. Confirmar que `$FE_BASE/cart/<token_A>` **ainda** carrega normal (status segue `active`).
4. Novo comentário de **outro** usuário para o mesmo produto.
5. Verificar: cart A `expired` (reservas `reversed`, estoque devolvido — `db-checks.md § Fase 8`); recarregar a página do cart A → tela de **carrinho expirado**; painel do evento reflete o status; cart B criado normalmente.

## Fase 9 — Encerrar o evento pela UI

Na página do evento: botão **"Finalizar evento"** → dialog de confirmação → **"Finalizar"**. Verificar: toast/resposta com `cartsFinalized`, evento `ended` na UI, carts `active`→`checkout` no banco (`FinalizeCartsByEvent`). Envio de links é best-effort (DM falha sem integração — esperado). Fallback API: `POST /api/v1/stores/:storeId/lives/:id/end`.

## Fase 10 — Relatório e cleanup

**Relatório final**, por fase: o que foi executado, o que a **UI mostrou** (screenshots: comentários no feed, carts na tabela, checkout pago, tela expirada), o que o **banco confirmou** (números: carts, reservas, estoque antes/depois, invariantes de `db-checks.md § Invariantes`) e o que ficou fora (caminhos do Modo B, DMs, ERP).

**Cleanup (só com confirmação do usuário)** — `live_events` cascateia para sessões, carts, itens e reservas:
```sql
DELETE FROM live_events WHERE id = '<EVENT_ID>';
DELETE FROM products WHERE id = '<PRODUCT_ID>' AND name LIKE '[E2E]%';
```
Em staging, propor o cleanup por padrão ao final. Se preferir manter os dados, apenas restaurar o estoque dos produtos de teste.
