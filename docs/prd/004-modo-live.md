# PRD 004 - Modo Live (Controle de Contexto)

**Status:** 🔴 Critico
**Prioridade:** P0
**Estimativa:** 5-7 dias

---

## 1. Visao Geral

### Problema
O vendedor precisa de um painel para controlar o contexto da live em tempo real:
- Qual produto esta sendo mostrado agora?
- Como interpretar comentarios simples como "quero" ou "eu"?
- Como pausar/resumir vendas durante a live?

Sem isso, o sistema so consegue detectar comentarios com keywords explicitas (ex: "ABCD"), perdendo vendas de usuarios que comentam apenas "quero".

### Solucao
Criar um painel de controle "Modo Live" onde o vendedor pode:
1. Definir produto ativo (contexto)
2. Trocar produto em tempo real
3. Pausar/resumir vendas
4. Visualizar vendas ao vivo

### Resultado Esperado
- Vendedor mostra produto X na camera
- Define produto X como "ativo" no painel
- Usuario comenta "quero"
- Sistema interpreta como "quero produto X"
- Carrinho criado automaticamente

---

## 2. Requisitos

### 2.1 Funcionais

| ID | Requisito | Prioridade |
|----|-----------|------------|
| RF01 | Definir produto ativo por evento | P0 |
| RF02 | Trocar produto ativo em tempo real | P0 |
| RF03 | Interpretar comentarios simples ("quero", "eu") | P0 |
| RF04 | Pausar/resumir vendas | P1 |
| RF05 | Visualizar vendas em tempo real | P1 |
| RF06 | Historico de produtos mostrados | P2 |
| RF07 | Multiplos produtos ativos simultaneos | P2 |

### 2.2 Nao-Funcionais

| ID | Requisito | Meta |
|----|-----------|------|
| RNF01 | Latencia de troca de contexto | < 500ms |
| RNF02 | Sincronizacao com worker | < 1s |
| RNF03 | Disponibilidade do painel | 99.9% |

---

## 3. Arquitetura

### 3.1 Componentes

```
┌─────────────────────────────────────────────────────────────┐
│                    PAINEL MODO LIVE (FE)                    │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐    │
│  │ Produto  │  │  Pausar  │  │  Vendas  │  │   Chat   │    │
│  │  Ativo   │  │  Vendas  │  │ Tempo Real│  │  Preview │    │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘    │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                      LIVE CONTEXT API                       │
│  PUT /events/:id/context                                    │
│  GET /events/:id/context                                    │
│  WS  /events/:id/live-feed                                  │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                    REDIS (Estado Global)                    │
│  live:context:{event_id} = {                                │
│    activeProductId: "uuid",                                 │
│    activeProductKeyword: "ABCD",                            │
│    isPaused: false,                                         │
│    updatedAt: "2024-01-01T00:00:00Z"                        │
│  }                                                          │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                    COMMENT WORKER                           │
│  1. Recebe comentario                                       │
│  2. Busca contexto do evento                                │
│  3. Se tem keyword explicita → usa keyword                  │
│  4. Se comentario generico → usa produto ativo              │
│  5. Cria/atualiza carrinho                                  │
└─────────────────────────────────────────────────────────────┘
```

### 3.2 Estado do Contexto

```go
type LiveContext struct {
    EventID             string    `json:"event_id"`
    ActiveProductID     *string   `json:"active_product_id"`
    ActiveProductName   *string   `json:"active_product_name"`
    ActiveProductKeyword *string  `json:"active_product_keyword"`
    IsPaused            bool      `json:"is_paused"`
    UpdatedAt           time.Time `json:"updated_at"`
    UpdatedBy           string    `json:"updated_by"` // user_id
}
```

### 3.3 Interpretacao de Comentarios

```go
type CommentIntent struct {
    Type        string   // "explicit_keyword", "generic_intent", "unknown"
    Keywords    []string // keywords encontradas
    IsIntent    bool     // indica intencao de compra
}

func ParseCommentIntent(text string) CommentIntent {
    // 1. Buscar keywords explicitas (ex: "ABCD", "quero ABCD")
    keywords := extractKeywords(text)
    if len(keywords) > 0 {
        return CommentIntent{Type: "explicit_keyword", Keywords: keywords}
    }

    // 2. Verificar intencao generica
    intentPhrases := []string{
        "quero", "eu quero", "manda", "me manda",
        "compro", "leva", "pega", "reserva",
        "eu", "esse", "essa", "isso",
        "🙋", "🙋‍♀️", "🙋‍♂️", "✋", "🖐️", "👋",
    }
    for _, phrase := range intentPhrases {
        if containsPhrase(text, phrase) {
            return CommentIntent{Type: "generic_intent", IsIntent: true}
        }
    }

    // 3. Desconhecido
    return CommentIntent{Type: "unknown"}
}
```

### 3.4 Fluxo de Processamento

```go
func (w *Worker) ProcessComment(ctx context.Context, comment Comment) error {
    // 1. Buscar contexto da live
    liveCtx, err := w.redis.GetLiveContext(ctx, comment.EventID)
    if err != nil {
        return err
    }

    // 2. Verificar se vendas estao pausadas
    if liveCtx.IsPaused {
        return nil // ignorar comentario
    }

    // 3. Parsear intencao do comentario
    intent := ParseCommentIntent(comment.Text)

    var productID string

    switch intent.Type {
    case "explicit_keyword":
        // Buscar produto pela keyword
        product, err := w.productRepo.GetByKeyword(ctx, intent.Keywords[0])
        if err != nil {
            return err
        }
        productID = product.ID

    case "generic_intent":
        // Usar produto ativo do contexto
        if liveCtx.ActiveProductID == nil {
            // Nenhum produto ativo, ignorar
            w.logger.Info("generic intent but no active product",
                zap.String("event_id", comment.EventID))
            return nil
        }
        productID = *liveCtx.ActiveProductID

    default:
        // Nao é um comentario de compra
        return nil
    }

    // 4. Criar/atualizar carrinho
    return w.cartService.AddItem(ctx, AddItemInput{
        EventID:        comment.EventID,
        PlatformUserID: comment.UserID,
        PlatformHandle: comment.Handle,
        ProductID:      productID,
        Quantity:       1,
    })
}
```

---

## 4. API

### 4.1 Endpoints

```
# Obter contexto atual
GET /api/v1/stores/:storeId/events/:eventId/context
Response: LiveContext

# Atualizar contexto
PUT /api/v1/stores/:storeId/events/:eventId/context
Body: {
    activeProductId: string | null,
    isPaused: boolean
}
Response: LiveContext

# WebSocket para feed em tempo real
WS /api/v1/stores/:storeId/events/:eventId/live-feed
Messages:
    - { type: "comment", data: Comment }
    - { type: "cart_created", data: Cart }
    - { type: "cart_updated", data: Cart }
    - { type: "payment_received", data: Payment }
    - { type: "context_changed", data: LiveContext }
```

### 4.2 Redis Keys

```
# Contexto da live
live:context:{event_id}
TTL: 24h

# Historico de produtos ativos
live:history:{event_id}
Type: List
Value: [{productId, startedAt, endedAt}, ...]

# Pub/Sub para atualizacoes
Channel: live:updates:{event_id}
```

---

## 5. Frontend

### 5.1 Pagina Modo Live

```
/events/[id]/live
```

### 5.2 Layout

```
┌─────────────────────────────────────────────────────────────────┐
│  🔴 LIVE: Black Friday 2024                    [Pausar] [Sair] │
├───────────────────────┬─────────────────────────────────────────┤
│                       │                                         │
│   PRODUTO ATIVO       │   COMENTARIOS EM TEMPO REAL             │
│   ┌─────────────┐     │   ┌─────────────────────────────────┐   │
│   │   [IMG]     │     │   │ @maria: quero!!!! 🙋            │   │
│   │  Camiseta   │     │   │ → Carrinho criado               │   │
│   │   R$ 89     │     │   │                                 │   │
│   │   #ABCD     │     │   │ @joao: EFGH                     │   │
│   └─────────────┘     │   │ → Item adicionado               │   │
│                       │   │                                 │   │
│   [Trocar Produto]    │   │ @ana: lindo!                    │   │
│                       │   │ → (nao é intencao de compra)    │   │
│   ──────────────────  │   └─────────────────────────────────┘   │
│                       │                                         │
│   VENDAS RAPIDAS      ├─────────────────────────────────────────┤
│   ┌─────┐ ┌─────┐     │   METRICAS EM TEMPO REAL                │
│   │EFGH │ │IJKL │     │   ┌─────────┐ ┌─────────┐ ┌─────────┐   │
│   │ +1  │ │ +1  │     │   │ Vendas  │ │Carrinho │ │ Receita │   │
│   └─────┘ └─────┘     │   │   12    │ │   28    │ │ R$ 2.4k │   │
│                       │   └─────────┘ └─────────┘ └─────────┘   │
└───────────────────────┴─────────────────────────────────────────┘
```

### 5.3 Componentes

```typescript
// components/live/LiveModePanel.tsx
interface LiveModePanelProps {
    eventId: string
}

// components/live/ActiveProductSelector.tsx
interface ActiveProductSelectorProps {
    products: Product[]
    activeProductId: string | null
    onSelect: (productId: string) => void
}

// components/live/LiveCommentFeed.tsx
interface LiveCommentFeedProps {
    eventId: string
    onCommentReceived: (comment: CommentWithAction) => void
}

// components/live/QuickProductButtons.tsx
// Botoes para trocar produto rapidamente
interface QuickProductButtonsProps {
    products: Product[]
    onProductClick: (productId: string) => void
}

// components/live/LiveMetrics.tsx
interface LiveMetricsProps {
    eventId: string
    // Atualiza em tempo real via WebSocket
}
```

### 5.4 WebSocket Hook

```typescript
// hooks/useLiveFeed.ts
function useLiveFeed(eventId: string) {
    const [comments, setComments] = useState<CommentWithAction[]>([])
    const [metrics, setMetrics] = useState<LiveMetrics>({})
    const [context, setContext] = useState<LiveContext | null>(null)

    useEffect(() => {
        const ws = new WebSocket(`/api/v1/events/${eventId}/live-feed`)

        ws.onmessage = (event) => {
            const message = JSON.parse(event.data)

            switch (message.type) {
                case "comment":
                    setComments(prev => [message.data, ...prev].slice(0, 100))
                    break
                case "cart_created":
                case "cart_updated":
                    updateMetrics(message.data)
                    break
                case "context_changed":
                    setContext(message.data)
                    break
            }
        }

        return () => ws.close()
    }, [eventId])

    return { comments, metrics, context }
}
```

---

## 6. Banco de Dados

### 6.1 Migration

```sql
-- Tabela de contexto de live (backup do Redis)
CREATE TABLE live_contexts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES live_events(id),
    active_product_id UUID REFERENCES products(id),
    is_paused BOOLEAN NOT NULL DEFAULT false,
    updated_by UUID REFERENCES users(id),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),

    UNIQUE(event_id)
);

-- Historico de produtos ativos
CREATE TABLE live_context_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES live_events(id),
    product_id UUID NOT NULL REFERENCES products(id),
    started_at TIMESTAMP WITH TIME ZONE NOT NULL,
    ended_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_live_context_history_event ON live_context_history(event_id);
```

---

## 7. Implementacao

### 7.1 Backend

#### Step 1: Redis Setup
- [ ] Adicionar Redis ao projeto (se nao existir)
- [ ] Criar client de Redis
- [ ] Implementar `LiveContextCache`

#### Step 2: Live Context Service
- [ ] Criar `internal/live/context_service.go`
- [ ] Implementar `GetContext`, `SetActiveProduct`, `TogglePause`
- [ ] Sincronizar Redis + DB

#### Step 3: Comment Intent Parser
- [ ] Criar `internal/live/intent_parser.go`
- [ ] Implementar deteccao de keywords
- [ ] Implementar deteccao de intencoes genericas
- [ ] Configurar frases por idioma/loja

#### Step 4: Atualizar Comment Worker
- [ ] Integrar com Live Context
- [ ] Usar produto ativo quando aplicavel
- [ ] Respeitar pause de vendas

#### Step 5: WebSocket
- [ ] Implementar endpoint WS para live feed
- [ ] Pub/Sub com Redis
- [ ] Broadcast de eventos

### 7.2 Frontend

- [ ] Criar pagina `/events/[id]/live`
- [ ] Implementar `LiveModePanel`
- [ ] Implementar `ActiveProductSelector`
- [ ] Implementar `LiveCommentFeed`
- [ ] Implementar `QuickProductButtons`
- [ ] Implementar `LiveMetrics`
- [ ] Integrar WebSocket

---

## 8. Testes

### 8.1 Unitarios

- [ ] `ParseCommentIntent` - todas as variantes
- [ ] `LiveContextCache` - get/set/expire
- [ ] Worker com contexto ativo

### 8.2 Integracao

- [ ] Trocar produto ativo → comentario processado corretamente
- [ ] Pausar vendas → comentarios ignorados
- [ ] WebSocket recebe atualizacoes

### 8.3 E2E

- [ ] Fluxo completo no painel
- [ ] Multiplos usuarios simultaneos

---

## 9. Metricas

| Metrica | Descricao |
|---------|-----------|
| `context_changes_count` | Trocas de produto ativo |
| `generic_intent_matches` | Comentarios genericos convertidos |
| `keyword_matches` | Comentarios com keyword explicita |
| `pause_duration_total` | Tempo total pausado |
| `live_mode_sessions` | Sessoes do painel abertas |

---

## 10. Riscos

| Risco | Mitigacao |
|-------|-----------|
| Latencia de sincronizacao | Redis com TTL curto |
| Produto errado detectado | Mostrar preview antes de aplicar |
| Vendedor esquece de trocar | Alerta visual quando produto muda na camera |
| Multiplos admins conflitando | Lock por usuario ou merge de acoes |

---

## 11. Checklist

- [ ] Redis configurado
- [ ] Backend implementado
- [ ] Frontend implementado
- [ ] WebSocket funcionando
- [ ] Testes passando
- [ ] Documentacao atualizada
