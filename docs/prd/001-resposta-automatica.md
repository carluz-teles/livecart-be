# PRD 001 - Resposta Automatica em Tempo Real

**Status:** 🔴 Critico
**Prioridade:** P0
**Estimativa:** 3-5 dias

---

## 1. Visao Geral

### Problema
Atualmente, o sistema detecta comentarios mas nao responde automaticamente ao usuario com o link de checkout. O usuario precisa esperar o fim da live para receber o link, o que causa:
- Perda de impulso de compra
- Menor conversao
- Experiencia ruim para o cliente

### Solucao
Responder automaticamente comentarios validos com link de checkout em menos de 3 segundos via DM ou reply no Instagram.

### Resultado Esperado
Usuario recebe link de checkout imediatamente apos comentar com keyword valida.

---

## 2. Requisitos

### 2.1 Funcionais

| ID | Requisito | Prioridade |
|----|-----------|------------|
| RF01 | Detectar comentario com keyword valida | P0 |
| RF02 | Gerar/atualizar carrinho em tempo real | P0 |
| RF03 | Gerar link de checkout persistente | P0 |
| RF04 | Enviar link via DM do Instagram | P0 |
| RF05 | Evitar duplicidade de envios | P0 |
| RF06 | Suportar reply publico como fallback | P1 |
| RF07 | Configurar mensagem personalizada por loja | P2 |

### 2.2 Nao-Funcionais

| ID | Requisito | Meta |
|----|-----------|------|
| RNF01 | Latencia fim-a-fim | < 3 segundos |
| RNF02 | Taxa de entrega de mensagens | > 95% |
| RNF03 | Idempotencia por comentario | 100% |
| RNF04 | Throughput | 100 comentarios/minuto |

---

## 3. Arquitetura

### 3.1 Fluxo Atual vs Proposto

```
ATUAL:
Webhook → Salva comentario → FIM (link so no final da live)

PROPOSTO:
Webhook → Fila (Redis) → Worker → Carrinho → Checkout → Notificacao
                                                    ↓
                                              DM Instagram
```

### 3.2 Componentes Novos

#### 3.2.1 Comment Queue (Redis)
```go
// Estrutura da mensagem na fila
type CommentJob struct {
    CommentID      string    `json:"comment_id"`
    EventID        string    `json:"event_id"`
    SessionID      string    `json:"session_id"`
    PlatformUserID string    `json:"platform_user_id"`
    PlatformHandle string    `json:"platform_handle"`
    Text           string    `json:"text"`
    Timestamp      time.Time `json:"timestamp"`
    RetryCount     int       `json:"retry_count"`
}
```

#### 3.2.2 Comment Worker
```go
type CommentWorker struct {
    queue         *redis.Client
    cartService   *cart.Service
    notifier      *InstagramNotifier
    logger        *zap.Logger
}

func (w *CommentWorker) Process(ctx context.Context, job CommentJob) error {
    // 1. Parse keywords do comentario
    // 2. Buscar/criar carrinho
    // 3. Adicionar itens
    // 4. Gerar checkout link
    // 5. Enviar DM
    // 6. Registrar evento de tracking
}
```

#### 3.2.3 Instagram Notifier (Atualizar)
```go
type NotifyCheckoutInput struct {
    PlatformUserID string
    PlatformHandle string
    CheckoutURL    string
    CartSummary    CartSummary
    StoreName      string
    MessageTemplate string // customizavel
}
```

### 3.3 Mudancas no Banco

```sql
-- Tabela para controle de idempotencia de notificacoes
CREATE TABLE comment_notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    comment_id VARCHAR(255) NOT NULL,
    event_id UUID NOT NULL REFERENCES live_events(id),
    platform_user_id VARCHAR(255) NOT NULL,
    notification_type VARCHAR(50) NOT NULL, -- 'dm', 'reply'
    status VARCHAR(50) NOT NULL, -- 'pending', 'sent', 'failed', 'skipped'
    checkout_url TEXT,
    sent_at TIMESTAMP WITH TIME ZONE,
    error_message TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),

    UNIQUE(comment_id, notification_type)
);

CREATE INDEX idx_comment_notifications_event ON comment_notifications(event_id);
CREATE INDEX idx_comment_notifications_user ON comment_notifications(platform_user_id);

-- Adicionar campo na tabela de stores para template de mensagem
ALTER TABLE stores ADD COLUMN checkout_message_template TEXT;
```

### 3.4 Configuracoes da Loja

```go
type CheckoutNotificationSettings struct {
    Enabled         bool   `json:"enabled"`
    SendViaDM       bool   `json:"send_via_dm"`        // default: true
    SendViaReply    bool   `json:"send_via_reply"`     // fallback
    MessageTemplate string `json:"message_template"`    // customizavel
    CooldownSeconds int    `json:"cooldown_seconds"`   // evitar spam (default: 30)
}
```

**Template padrao:**
```
Oi {{handle}}! 🛒

Seu carrinho esta pronto:
{{#items}}
• {{name}} - {{quantity}}x
{{/items}}

Total: {{total}}

Finalize aqui: {{checkout_url}}

Valido por {{expiry_hours}}h
```

---

## 4. Implementacao

### 4.1 Backend

#### Step 1: Redis Queue Setup
- [ ] Adicionar Redis ao docker-compose
- [ ] Criar cliente Redis no main.go
- [ ] Implementar `CommentQueue` com push/pop

#### Step 2: Comment Worker
- [ ] Criar `internal/worker/comment_worker.go`
- [ ] Implementar processamento de jobs
- [ ] Adicionar retry com backoff exponencial
- [ ] Integrar com cart service

#### Step 3: Notificacao Instagram
- [ ] Atualizar `notifier_instagram.go` para DM imediato
- [ ] Implementar fallback para reply
- [ ] Adicionar rate limiting (Instagram API limits)

#### Step 4: Idempotencia
- [ ] Criar migration para `comment_notifications`
- [ ] Implementar check antes de enviar
- [ ] Registrar todas as tentativas

#### Step 5: Configuracoes
- [ ] Adicionar campos de template na store
- [ ] Criar endpoint para atualizar configuracoes
- [ ] Implementar rendering de template

### 4.2 Mudancas no Webhook Handler

```go
// instagram_handler.go - ANTES
func (h *Handler) HandleComment(ctx context.Context, comment Comment) error {
    // Salva comentario no banco
    return h.repo.SaveComment(ctx, comment)
}

// instagram_handler.go - DEPOIS
func (h *Handler) HandleComment(ctx context.Context, comment Comment) error {
    // 1. Salva comentario no banco (manter)
    if err := h.repo.SaveComment(ctx, comment); err != nil {
        return err
    }

    // 2. Enfileira para processamento assincrono (NOVO)
    job := CommentJob{
        CommentID:      comment.ID,
        EventID:        comment.EventID,
        // ...
    }
    return h.queue.Push(ctx, job)
}
```

### 4.3 Frontend (Configuracoes)

- [ ] Adicionar secao em `/settings/cart` para notificacoes
- [ ] Campo de toggle para DM automatico
- [ ] Editor de template de mensagem
- [ ] Preview da mensagem

---

## 5. API Endpoints

### 5.1 Novos Endpoints

```
# Configuracoes de notificacao
GET  /api/v1/stores/:storeId/notification-settings
PUT  /api/v1/stores/:storeId/notification-settings

# Preview de template
POST /api/v1/stores/:storeId/notification-settings/preview
```

### 5.2 Schemas

```typescript
// NotificationSettings
interface NotificationSettings {
  enabled: boolean
  sendViaDM: boolean
  sendViaReply: boolean
  messageTemplate: string
  cooldownSeconds: number
}

// PreviewRequest
interface PreviewRequest {
  template: string
  sampleData?: {
    handle: string
    items: Array<{ name: string; quantity: number }>
    total: number
  }
}

// PreviewResponse
interface PreviewResponse {
  renderedMessage: string
}
```

---

## 6. Testes

### 6.1 Unitarios

- [ ] `CommentQueue.Push/Pop`
- [ ] `CommentWorker.Process`
- [ ] Template rendering
- [ ] Idempotencia check

### 6.2 Integracao

- [ ] Webhook → Queue → Worker → Notificacao
- [ ] Retry em caso de falha
- [ ] Rate limiting

### 6.3 E2E

- [ ] Comentario real → DM recebida
- [ ] Multiplos comentarios do mesmo usuario
- [ ] Cooldown entre mensagens

---

## 7. Metricas

### 7.1 SLIs

| Metrica | Target |
|---------|--------|
| Latencia P50 | < 1.5s |
| Latencia P95 | < 3s |
| Taxa de sucesso | > 95% |
| Taxa de duplicidade | < 0.1% |

### 7.2 Tracking Events

```go
const (
    EventCommentReceived    = "comment.received"
    EventCommentQueued      = "comment.queued"
    EventCommentProcessed   = "comment.processed"
    EventNotificationSent   = "notification.sent"
    EventNotificationFailed = "notification.failed"
)
```

---

## 8. Rollout

### 8.1 Fases

1. **Alpha (1 semana)**
   - Habilitar para 1-2 lojas de teste
   - Monitorar latencia e erros

2. **Beta (1 semana)**
   - Habilitar para 10% das lojas
   - Coletar feedback

3. **GA**
   - Rollout completo
   - Feature flag para disable

### 8.2 Feature Flags

```go
const (
    FlagAutoNotifyEnabled = "auto_notify_enabled"
    FlagAutoNotifyDM      = "auto_notify_dm"
)
```

---

## 9. Dependencias

| Dependencia | Status | Responsavel |
|-------------|--------|-------------|
| Redis setup | Pendente | Infra |
| Instagram DM API permissions | Verificar | Integracao |
| Rate limits Instagram | Documentar | Integracao |

---

## 10. Riscos e Mitigacoes

| Risco | Impacto | Mitigacao |
|-------|---------|-----------|
| Rate limit Instagram | Alto | Implementar queue com backoff |
| Spam detection Instagram | Alto | Cooldown entre mensagens |
| Latencia alta | Medio | Worker pool, Redis local |
| Falha no envio | Medio | Retry + fallback reply |

---

## 11. Checklist de Entrega

- [ ] Codigo implementado
- [ ] Testes passando
- [ ] Documentacao API atualizada
- [ ] Metricas configuradas
- [ ] Alertas configurados
- [ ] Rollout plan aprovado
- [ ] Feature flag configurada
