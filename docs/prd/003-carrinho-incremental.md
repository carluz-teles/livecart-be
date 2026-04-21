# PRD 003 - Carrinho Incremental

**Status:** 🔴 Critico
**Prioridade:** P0
**Estimativa:** 2-3 dias

---

## 1. Visao Geral

### Problema
Verificar se o sistema ja suporta carrinho incremental ou se cada comentario cria um carrinho novo.

### Objetivo
Garantir que um usuario possa acumular multiplos produtos ao longo da live atraves de comentarios sucessivos, consolidando tudo em um unico checkout.

### Resultado Esperado
- Usuario comenta "ABC" → 1 item no carrinho
- Usuario comenta "XYZ" → 2 itens no carrinho (mesmo carrinho)
- 1 checkout com todos os itens

---

## 2. Analise do Estado Atual

### 2.1 Verificacao Necessaria

Precisamos verificar no codigo atual:

```go
// Onde o carrinho é criado/atualizado?
// apps/api/internal/live/service.go ou integration/service.go

// Logica esperada:
func ProcessComment(ctx context.Context, comment Comment) error {
    // 1. Buscar carrinho existente por (event_id + platform_user_id)
    cart, err := repo.GetCartByEventAndUser(ctx, comment.EventID, comment.PlatformUserID)

    if cart == nil {
        // 2a. Se não existe, criar novo
        cart = createNewCart(...)
    }

    // 2b. Se existe, adicionar item
    addItemToCart(cart, product)
}
```

### 2.2 Query SQL Existente

Verificar em `apps/api/db/queries/cart.sql`:

```sql
-- Existe query para buscar por event + user?
-- name: GetCartByEventAndUser :one
SELECT * FROM carts
WHERE event_id = $1 AND platform_user_id = $2
AND status IN ('active', 'pending')
LIMIT 1;
```

### 2.3 Chave do Carrinho

A chave unica do carrinho deve ser:
```
(event_id, platform_user_id)
```

Nao deve ser:
```
(event_id, platform_user_id, comment_id)  -- ERRADO: criaria carrinho por comentario
(session_id, platform_user_id)             -- ERRADO: crash recovery quebraria
```

---

## 3. Requisitos

### 3.1 Funcionais

| ID | Requisito | Status |
|----|-----------|--------|
| RF01 | Identificar usuario unico por `platform_user_id` | Verificar |
| RF02 | Manter carrinho ativo durante toda a sessao | Verificar |
| RF03 | Adicionar itens via comentarios sucessivos | Verificar |
| RF04 | Consolidar todos os itens em 1 checkout | Verificar |
| RF05 | Atualizar quantidade se mesmo produto | Verificar |

### 3.2 Nao-Funcionais

| ID | Requisito | Meta |
|----|-----------|------|
| RNF01 | Concorrencia | Suportar comentarios simultaneos |
| RNF02 | Consistencia | Carrinho sempre reflete todos os itens |
| RNF03 | Sincronizacao | Redis + DB em sync |

---

## 4. Arquitetura

### 4.1 Fluxo de Adicao de Item

```
Comentario recebido
        ↓
Extrair: event_id, platform_user_id, keyword
        ↓
Buscar produto por keyword
        ↓
┌─────────────────────────────────────┐
│  GetCartByEventAndUser              │
│  (event_id, platform_user_id)       │
└─────────────────────────────────────┘
        ↓
   Carrinho existe?
    /           \
  SIM            NAO
   ↓              ↓
UpsertItem    CreateCart + AddItem
   ↓              ↓
└──────────┬──────┘
           ↓
   Atualizar totais
           ↓
   Sincronizar reserva de estoque
           ↓
   Notificar usuario (se primeira vez ou config)
```

### 4.2 Estrutura de Dados

```go
// Carrinho identificado por event + user
type CartKey struct {
    EventID        string
    PlatformUserID string
}

// Item no carrinho
type CartItem struct {
    ProductID  string
    Quantity   int
    UnitPrice  int64  // em centavos
    Waitlisted bool
}

// Operacao de upsert
type UpsertCartItemInput struct {
    CartID    string
    ProductID string
    Quantity  int  // quantidade a ADICIONAR (nao total)
}
```

### 4.3 Concorrencia

```go
// Para evitar race conditions, usar transacao ou lock

func (s *Service) AddItemToCart(ctx context.Context, input AddItemInput) error {
    // Opcao 1: Lock otimista com retry
    for retries := 0; retries < 3; retries++ {
        cart, version := s.repo.GetCartWithVersion(ctx, input.CartKey)

        // Verificar estoque
        if !s.hasStock(input.ProductID, input.Quantity) {
            return ErrOutOfStock
        }

        // Tentar atualizar
        err := s.repo.UpsertItemWithVersion(ctx, cart.ID, input, version)
        if err == ErrVersionConflict {
            continue // retry
        }
        return err
    }
    return ErrConcurrencyLimit

    // Opcao 2: Lock pessimista (Redis)
    lock := s.redis.Lock(fmt.Sprintf("cart:%s", input.CartKey))
    defer lock.Unlock()
    // ... operacao ...
}
```

---

## 5. Implementacao

### 5.1 Verificar Codigo Existente

```bash
# Buscar onde carrinho é criado
grep -r "CreateCart" apps/api/internal/

# Buscar logica de adicao de item
grep -r "UpsertCartItem" apps/api/internal/

# Verificar query de busca por event + user
grep -r "GetCartByEventAndUser" apps/api/
```

### 5.2 SQL Queries Necessarias

```sql
-- name: GetCartByEventAndUser :one
SELECT * FROM carts
WHERE event_id = $1
  AND platform_user_id = $2
  AND status IN ('active', 'checkout')
ORDER BY created_at DESC
LIMIT 1;

-- name: UpsertCartItem :one
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price)
VALUES ($1, $2, $3, $4)
ON CONFLICT (cart_id, product_id)
DO UPDATE SET
    quantity = cart_items.quantity + EXCLUDED.quantity,
    updated_at = NOW()
RETURNING *;

-- name: GetCartItemsTotal :one
SELECT
    COUNT(*) as item_count,
    SUM(quantity) as total_units,
    SUM(quantity * unit_price) as total_value
FROM cart_items
WHERE cart_id = $1;
```

### 5.3 Steps de Implementacao

Se carrinho incremental NAO existe:

- [ ] Adicionar query `GetCartByEventAndUser`
- [ ] Modificar handler de comentario para buscar carrinho existente
- [ ] Implementar `UpsertCartItem` com incremento de quantidade
- [ ] Adicionar lock para concorrencia
- [ ] Atualizar notificacao para enviar apenas 1x

Se carrinho incremental JA existe:

- [ ] Verificar se funciona corretamente
- [ ] Verificar comportamento de quantidade (incrementa ou substitui?)
- [ ] Verificar sincronizacao de estoque
- [ ] Adicionar testes

---

## 6. Cenarios de Teste

### 6.1 Cenario: Multiplos Produtos

```
Usuario: @joao
Live: evento_123

Comentario 1: "quero ABCD"
→ Carrinho criado: [ABCD x1]

Comentario 2: "quero EFGH"
→ Carrinho atualizado: [ABCD x1, EFGH x1]

Comentario 3: "ABCD"
→ Carrinho atualizado: [ABCD x2, EFGH x1]  // quantidade incrementada

Checkout: 3 itens, 1 pagamento
```

### 6.2 Cenario: Comentarios Rapidos

```
Usuario: @maria
Envia 5 comentarios em 2 segundos:
- "AAAA"
- "BBBB"
- "CCCC"
- "DDDD"
- "EEEE"

Esperado: 1 carrinho com 5 produtos
Risco: Race condition criando carrinhos duplicados
```

### 6.3 Cenario: Produto Sem Estoque

```
Usuario: @pedro
Produto ABCD: estoque = 1

Comentario 1: "ABCD"
→ Carrinho: [ABCD x1], estoque reservado

Comentario 2: "ABCD"
→ Opcao A: Erro, ja nao tem estoque
→ Opcao B: Adiciona a waitlist
→ Opcao C: Ignora silenciosamente
```

---

## 7. Comportamento de Quantidade

### 7.1 Opcao A: Sempre Incrementar (Recomendado)

```
"ABCD" → +1
"ABCD" → +1
Total: 2 unidades
```

**Pros:** Simples, intuitivo para lives rapidas
**Cons:** Usuario pode pedir mais do que quer

### 7.2 Opcao B: Numero Explicito

```
"ABCD 3" → 3 unidades (substitui)
"ABCD"   → 1 unidade (default)
```

**Pros:** Controle preciso
**Cons:** Mais complexo de parsear

### 7.3 Opcao C: Configuravel por Loja

```go
type CartBehavior struct {
    DuplicateKeyword string // "increment" | "replace" | "ignore"
    DefaultQuantity  int    // 1
    AllowExplicitQty bool   // true = permite "ABCD 3"
}
```

---

## 8. Redis para Performance

### 8.1 Cache de Carrinho Ativo

```go
// Chave: cart:active:{event_id}:{platform_user_id}
// Valor: cart_id
// TTL: duracao do evento + margem

func (r *RedisCache) GetActiveCartID(eventID, userID string) (string, error) {
    key := fmt.Sprintf("cart:active:%s:%s", eventID, userID)
    return r.client.Get(ctx, key).Result()
}

func (r *RedisCache) SetActiveCartID(eventID, userID, cartID string) error {
    key := fmt.Sprintf("cart:active:%s:%s", eventID, userID)
    return r.client.Set(ctx, key, cartID, 2*time.Hour).Err()
}
```

### 8.2 Lock Distribuido

```go
func (s *Service) AddItemWithLock(ctx context.Context, input AddItemInput) error {
    lockKey := fmt.Sprintf("lock:cart:%s:%s", input.EventID, input.UserID)

    // Acquire lock
    ok, err := s.redis.SetNX(ctx, lockKey, "1", 5*time.Second).Result()
    if !ok {
        return ErrCartLocked
    }
    defer s.redis.Del(ctx, lockKey)

    // Process
    return s.addItem(ctx, input)
}
```

---

## 9. Metricas

| Metrica | Descricao |
|---------|-----------|
| `items_per_cart_avg` | Media de itens por carrinho |
| `items_per_cart_max` | Maximo de itens em um carrinho |
| `cart_updates_count` | Atualizacoes de carrinho (vs criacao) |
| `duplicate_keyword_count` | Mesmo produto pedido 2+ vezes |
| `race_condition_retries` | Retries por concorrencia |

---

## 10. Checklist

### Verificacao

- [ ] Analisar codigo atual de criacao de carrinho
- [ ] Verificar query `GetCartByEventAndUser`
- [ ] Verificar comportamento de `UpsertCartItem`
- [ ] Testar com multiplos comentarios

### Implementacao (se necessario)

- [ ] Implementar busca de carrinho por event + user
- [ ] Implementar upsert com incremento
- [ ] Adicionar lock para concorrencia
- [ ] Integrar com Redis (cache + lock)
- [ ] Testes de concorrencia
