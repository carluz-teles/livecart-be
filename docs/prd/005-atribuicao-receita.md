# PRD 005 - Atribuicao de Receita

**Status:** 🔴 Critico
**Prioridade:** P0
**Estimativa:** 3-4 dias

---

## 1. Visao Geral

### Problema
Nao temos uma forma clara de medir quanto do faturamento foi gerado pelo sistema LiveCart. Isso dificulta:
- Demonstrar valor para o cliente
- Medir ROI
- Identificar oportunidades de melhoria
- Calcular comissoes (se aplicavel)

### Solucao
Implementar sistema de tracking de eventos que permita atribuir cada venda ao fluxo do LiveCart, desde o comentario ate o pagamento.

### Resultado Esperado
- "Essa live gerou R$ X via LiveCart"
- "Y% dos comentarios converteram em compra"
- "Tempo medio de conversao: Z minutos"

---

## 2. Requisitos

### 2.1 Funcionais

| ID | Requisito | Prioridade |
|----|-----------|------------|
| RF01 | Rastrear origem de cada compra | P0 |
| RF02 | Vincular pedido ao evento/live | P0 |
| RF03 | Calcular GMV por live | P0 |
| RF04 | Medir taxa de conversao | P0 |
| RF05 | Tempo ate compra (comentario → pagamento) | P1 |
| RF06 | Funil de conversao completo | P1 |
| RF07 | Comparativo entre lives | P2 |

### 2.2 Metricas a Rastrear

| Metrica | Descricao | Calculo |
|---------|-----------|---------|
| GMV | Gross Merchandise Value | Soma de pagamentos confirmados |
| CVR | Conversion Rate | Carrinhos pagos / Total carrinhos |
| Comment CVR | Taxa de comentarios | Carrinhos / Comentarios validos |
| AOV | Average Order Value | GMV / Numero de pedidos |
| Time to Purchase | Tempo ate compra | Avg(pagamento - primeiro_comentario) |
| Items per Cart | Itens por carrinho | Avg(total_items) |

---

## 3. Arquitetura

### 3.1 Event Tracking

```go
// Eventos a rastrear
const (
    // Comentarios
    EventCommentReceived      = "comment.received"
    EventCommentValidKeyword  = "comment.valid_keyword"
    EventCommentGenericIntent = "comment.generic_intent"
    EventCommentIgnored       = "comment.ignored"

    // Carrinho
    EventCartCreated     = "cart.created"
    EventCartItemAdded   = "cart.item_added"
    EventCartItemRemoved = "cart.item_removed"
    EventCartExpired     = "cart.expired"

    // Checkout
    EventCheckoutStarted   = "checkout.started"
    EventCheckoutAbandoned = "checkout.abandoned"

    // Pagamento
    EventPaymentInitiated = "payment.initiated"
    EventPaymentCompleted = "payment.completed"
    EventPaymentFailed    = "payment.failed"
    EventPaymentRefunded  = "payment.refunded"
)
```

### 3.2 Estrutura do Evento

```go
type TrackingEvent struct {
    ID              string                 `json:"id"`
    Type            string                 `json:"type"`
    Timestamp       time.Time              `json:"timestamp"`
    CorrelationID   string                 `json:"correlation_id"` // agrupa eventos relacionados
    StoreID         string                 `json:"store_id"`
    EventID         string                 `json:"event_id"`       // live event
    SessionID       *string                `json:"session_id"`
    CartID          *string                `json:"cart_id"`
    PlatformUserID  *string                `json:"platform_user_id"`
    PlatformHandle  *string                `json:"platform_handle"`
    ProductID       *string                `json:"product_id"`
    AmountCents     *int64                 `json:"amount_cents"`
    Metadata        map[string]interface{} `json:"metadata"`
}
```

### 3.3 Correlation ID

Cada fluxo de usuario recebe um `correlation_id` unico que conecta todos os eventos:

```
correlation_id = hash(event_id + platform_user_id)

Exemplo de fluxo com mesmo correlation_id:
1. comment.received      (12:00:00)
2. comment.valid_keyword (12:00:01)
3. cart.created          (12:00:02)
4. cart.item_added       (12:00:02)
5. checkout.started      (12:05:00)
6. payment.initiated     (12:06:00)
7. payment.completed     (12:06:30)
```

### 3.4 Banco de Dados

```sql
-- Tabela de eventos de tracking
CREATE TABLE tracking_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type VARCHAR(100) NOT NULL,
    correlation_id VARCHAR(255) NOT NULL,
    store_id UUID NOT NULL REFERENCES stores(id),
    event_id UUID REFERENCES live_events(id),
    session_id UUID REFERENCES live_sessions(id),
    cart_id UUID REFERENCES carts(id),
    platform_user_id VARCHAR(255),
    platform_handle VARCHAR(255),
    product_id UUID REFERENCES products(id),
    amount_cents BIGINT,
    metadata JSONB,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Indices para queries de analytics
CREATE INDEX idx_tracking_events_type ON tracking_events(type);
CREATE INDEX idx_tracking_events_correlation ON tracking_events(correlation_id);
CREATE INDEX idx_tracking_events_store ON tracking_events(store_id);
CREATE INDEX idx_tracking_events_event ON tracking_events(event_id);
CREATE INDEX idx_tracking_events_created ON tracking_events(created_at);
CREATE INDEX idx_tracking_events_store_type_created ON tracking_events(store_id, type, created_at);

-- View materializada para metricas por evento
CREATE MATERIALIZED VIEW event_metrics AS
SELECT
    e.id as event_id,
    e.store_id,
    e.title,
    e.created_at as event_date,

    -- Comentarios
    COUNT(DISTINCT CASE WHEN t.type = 'comment.received' THEN t.id END) as total_comments,
    COUNT(DISTINCT CASE WHEN t.type = 'comment.valid_keyword' THEN t.id END) as valid_comments,

    -- Carrinhos
    COUNT(DISTINCT CASE WHEN t.type = 'cart.created' THEN t.cart_id END) as carts_created,

    -- Checkouts
    COUNT(DISTINCT CASE WHEN t.type = 'checkout.started' THEN t.cart_id END) as checkouts_started,

    -- Pagamentos
    COUNT(DISTINCT CASE WHEN t.type = 'payment.completed' THEN t.cart_id END) as payments_completed,
    COALESCE(SUM(CASE WHEN t.type = 'payment.completed' THEN t.amount_cents END), 0) as gmv_cents,

    -- Metricas derivadas
    COUNT(DISTINCT t.platform_user_id) as unique_users

FROM live_events e
LEFT JOIN tracking_events t ON t.event_id = e.id
GROUP BY e.id, e.store_id, e.title, e.created_at;

CREATE UNIQUE INDEX idx_event_metrics_event ON event_metrics(event_id);

-- Refresh da view (rodar periodicamente)
-- REFRESH MATERIALIZED VIEW CONCURRENTLY event_metrics;
```

---

## 4. Implementacao

### 4.1 Tracking Service

```go
// internal/tracking/service.go

type Service struct {
    repo   *Repository
    logger *zap.Logger
}

func (s *Service) Track(ctx context.Context, event TrackingEvent) error {
    // Validar evento
    if err := validateEvent(event); err != nil {
        return err
    }

    // Enriquecer com timestamp se nao tiver
    if event.Timestamp.IsZero() {
        event.Timestamp = time.Now()
    }

    // Salvar no banco
    return s.repo.Create(ctx, event)
}

func (s *Service) TrackAsync(event TrackingEvent) {
    // Enviar para fila para processamento assincrono
    // Nao bloqueia o fluxo principal
    go func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        if err := s.Track(ctx, event); err != nil {
            s.logger.Error("failed to track event", zap.Error(err))
        }
    }()
}
```

### 4.2 Integracao nos Servicos

```go
// live/service.go - ao processar comentario
func (s *Service) ProcessComment(ctx context.Context, comment Comment) error {
    correlationID := generateCorrelationID(comment.EventID, comment.UserID)

    // Track comentario recebido
    s.tracker.TrackAsync(TrackingEvent{
        Type:           EventCommentReceived,
        CorrelationID:  correlationID,
        EventID:        comment.EventID,
        PlatformUserID: comment.UserID,
        PlatformHandle: comment.Handle,
        Metadata: map[string]interface{}{
            "text": comment.Text,
        },
    })

    // Processar...
    intent := parseIntent(comment.Text)

    if intent.IsValid {
        s.tracker.TrackAsync(TrackingEvent{
            Type:          EventCommentValidKeyword,
            CorrelationID: correlationID,
            EventID:       comment.EventID,
            Metadata: map[string]interface{}{
                "keywords": intent.Keywords,
            },
        })
    }
    // ...
}

// checkout/service.go - ao processar pagamento
func (s *Service) ProcessPayment(ctx context.Context, input PaymentInput) error {
    correlationID := getCorrelationIDFromCart(input.CartID)

    s.tracker.Track(ctx, TrackingEvent{
        Type:          EventPaymentCompleted,
        CorrelationID: correlationID,
        CartID:        input.CartID,
        AmountCents:   input.Amount,
    })
    // ...
}
```

### 4.3 Analytics Service

```go
// internal/analytics/service.go

type EventAnalytics struct {
    EventID          string  `json:"event_id"`
    Title            string  `json:"title"`
    Date             string  `json:"date"`
    TotalComments    int     `json:"total_comments"`
    ValidComments    int     `json:"valid_comments"`
    CartsCreated     int     `json:"carts_created"`
    CheckoutsStarted int     `json:"checkouts_started"`
    PaymentsCompleted int    `json:"payments_completed"`
    GMV              int64   `json:"gmv"`              // em centavos
    UniqueUsers      int     `json:"unique_users"`
    ConversionRate   float64 `json:"conversion_rate"` // pagamentos / carrinhos
    CommentCVR       float64 `json:"comment_cvr"`     // carrinhos / comentarios validos
    AOV              int64   `json:"aov"`             // GMV / pagamentos
}

func (s *Service) GetEventAnalytics(ctx context.Context, eventID string) (*EventAnalytics, error) {
    // Query da view materializada ou calcular em tempo real
    return s.repo.GetEventMetrics(ctx, eventID)
}

func (s *Service) GetStoreAnalytics(ctx context.Context, storeID string, period Period) (*StoreAnalytics, error) {
    // Agregar metricas de todos os eventos do periodo
    return s.repo.GetStoreMetrics(ctx, storeID, period)
}
```

---

## 5. API

### 5.1 Endpoints

```
# Metricas de um evento
GET /api/v1/stores/:storeId/events/:eventId/analytics
Response: EventAnalytics

# Metricas da loja (agregado)
GET /api/v1/stores/:storeId/analytics
Query: ?period=7d|30d|90d|all
Response: StoreAnalytics

# Funil de conversao de um evento
GET /api/v1/stores/:storeId/events/:eventId/funnel
Response: ConversionFunnel

# Timeline de eventos (debug)
GET /api/v1/stores/:storeId/events/:eventId/timeline
Query: ?correlation_id=xxx
Response: TrackingEvent[]
```

### 5.2 Schemas

```typescript
interface EventAnalytics {
    eventId: string
    title: string
    date: string

    // Comentarios
    totalComments: number
    validComments: number

    // Carrinhos
    cartsCreated: number
    cartsExpired: number

    // Checkouts
    checkoutsStarted: number
    checkoutsAbandoned: number

    // Pagamentos
    paymentsCompleted: number
    paymentsFailed: number
    gmv: number // centavos

    // Derivadas
    uniqueUsers: number
    conversionRate: number // 0-1
    commentCVR: number     // 0-1
    aov: number           // centavos
    avgTimeToCheckout: number // segundos
    avgTimeToPurchase: number // segundos
}

interface ConversionFunnel {
    eventId: string
    steps: FunnelStep[]
}

interface FunnelStep {
    name: string
    count: number
    percentage: number // em relacao ao passo anterior
    dropoff: number    // % que nao avancou
}

// Exemplo de funil:
// 1. Comentarios Validos: 100 (100%)
// 2. Carrinhos Criados: 45 (45%)
// 3. Checkout Iniciado: 30 (67%)
// 4. Pagamento Completo: 20 (67%)
```

---

## 6. Frontend

### 6.1 Dashboard de Evento

Adicionar secao de analytics na pagina `/events/[id]`:

```tsx
// components/event/EventAnalytics.tsx
function EventAnalytics({ eventId }: { eventId: string }) {
    const { data: analytics } = useEventAnalytics(eventId)

    return (
        <Card>
            <CardHeader>
                <CardTitle>Performance da Live</CardTitle>
            </CardHeader>
            <CardContent>
                <div className="grid grid-cols-4 gap-4">
                    <StatCard
                        title="GMV"
                        value={formatCurrency(analytics.gmv)}
                        icon={<DollarSign />}
                    />
                    <StatCard
                        title="Conversao"
                        value={`${(analytics.conversionRate * 100).toFixed(1)}%`}
                        icon={<TrendingUp />}
                    />
                    <StatCard
                        title="Pedidos"
                        value={analytics.paymentsCompleted}
                        icon={<ShoppingCart />}
                    />
                    <StatCard
                        title="Ticket Medio"
                        value={formatCurrency(analytics.aov)}
                        icon={<Receipt />}
                    />
                </div>

                <ConversionFunnelChart eventId={eventId} />
            </CardContent>
        </Card>
    )
}
```

### 6.2 Funil de Conversao

```tsx
// components/event/ConversionFunnel.tsx
function ConversionFunnelChart({ eventId }: { eventId: string }) {
    const { data: funnel } = useEventFunnel(eventId)

    return (
        <div className="mt-6">
            <h4 className="font-medium mb-4">Funil de Conversao</h4>
            <div className="space-y-2">
                {funnel.steps.map((step, i) => (
                    <FunnelStep
                        key={step.name}
                        name={step.name}
                        count={step.count}
                        percentage={step.percentage}
                        isFirst={i === 0}
                    />
                ))}
            </div>
        </div>
    )
}

function FunnelStep({ name, count, percentage, isFirst }) {
    return (
        <div className="flex items-center gap-4">
            <div
                className="h-8 bg-primary/20 rounded"
                style={{ width: `${percentage * 100}%` }}
            />
            <div className="flex-1">
                <span className="font-medium">{name}</span>
                <span className="text-muted-foreground ml-2">
                    {count} ({isFirst ? '100%' : `${(percentage * 100).toFixed(0)}%`})
                </span>
            </div>
        </div>
    )
}
```

---

## 7. Implementacao

### 7.1 Steps Backend

- [ ] Criar tabela `tracking_events`
- [ ] Criar view materializada `event_metrics`
- [ ] Implementar `tracking.Service`
- [ ] Integrar tracking no comment worker
- [ ] Integrar tracking no cart service
- [ ] Integrar tracking no checkout service
- [ ] Implementar `analytics.Service`
- [ ] Criar endpoints de analytics
- [ ] Configurar job para refresh da view

### 7.2 Steps Frontend

- [ ] Criar hooks `useEventAnalytics`, `useEventFunnel`
- [ ] Implementar `EventAnalytics` component
- [ ] Implementar `ConversionFunnel` component
- [ ] Adicionar na pagina de detalhes do evento
- [ ] Adicionar comparativo no dashboard principal

---

## 8. Testes

### 8.1 Unitarios

- [ ] Geracao de correlation_id
- [ ] Validacao de eventos
- [ ] Calculos de metricas

### 8.2 Integracao

- [ ] Tracking end-to-end
- [ ] Query de analytics
- [ ] Refresh de view

---

## 9. Metricas de Monitoramento

| Metrica | Descricao |
|---------|-----------|
| `tracking_events_per_minute` | Volume de eventos |
| `tracking_latency_p95` | Latencia de gravacao |
| `analytics_query_time_p95` | Tempo de query |
| `view_refresh_time` | Tempo de refresh da view |

---

## 10. Checklist

- [ ] Schema de banco criado
- [ ] Tracking service implementado
- [ ] Integracoes com servicos existentes
- [ ] Analytics service implementado
- [ ] API endpoints criados
- [ ] Frontend implementado
- [ ] View materializada configurada
- [ ] Job de refresh configurado
- [ ] Testes passando
