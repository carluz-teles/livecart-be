# PRD 002 - Checkout Durante a Live

**Status:** 🔴 Critico
**Prioridade:** P0
**Estimativa:** 2-3 dias

---

## 1. Visao Geral

### Problema
Atualmente o checkout so fica disponivel apos o evento ser finalizado:
- Carrinho criado durante live tem status `pending`
- Checkout so funciona quando status = `checkout`
- Usuario precisa esperar fim da live para pagar

Isso causa:
- Perda do impulso de compra
- Menor conversao
- Usuario pode esquecer/desistir

### Solucao
Permitir checkout a qualquer momento durante a live, com carrinho dinamico que reflete atualizacoes em tempo real.

### Resultado Esperado
Usuario pode acessar checkout e pagar enquanto a live esta acontecendo.

---

## 2. Analise do Estado Atual

### 2.1 Fluxo Atual

```
Comentario detectado
        ↓
Carrinho criado (status: pending)
        ↓
[LIVE ACONTECENDO - CARRINHO BLOQUEADO]
        ↓
Evento finalizado
        ↓
Carrinho atualizado (status: checkout)
        ↓
Link enviado ao cliente
        ↓
Cliente pode pagar
```

### 2.2 Codigo Atual (checkout/service.go)

```go
func (s *Service) GetCartForCheckout(ctx context.Context, input GetCartForCheckoutInput) (*CartForCheckoutResponse, error) {
    cart, err := s.repo.GetCartByToken(ctx, input.Token)
    if err != nil {
        return nil, err
    }

    // BLOQUEIO ATUAL - so permite checkout se status != pending
    if cart.Status == "pending" {
        return nil, httpx.ErrBadRequest("Carrinho ainda não está pronto para checkout")
    }
    // ...
}
```

### 2.3 Status do Carrinho

| Status | Descricao | Checkout Permitido |
|--------|-----------|-------------------|
| `pending` | Live em andamento | ❌ Nao |
| `checkout` | Pronto para pagar | ✅ Sim |
| `expired` | Expirou | ❌ Nao |
| `paid` | Pago | ❌ Nao |

---

## 3. Solucao Proposta

### 3.1 Novo Fluxo

```
Comentario detectado
        ↓
Carrinho criado (status: active) ← NOVO STATUS
        ↓
Link de checkout enviado imediatamente
        ↓
[LIVE ACONTECENDO]
  ├── Cliente pode pagar a qualquer momento
  └── Carrinho continua recebendo itens
        ↓
Evento finalizado
        ↓
Carrinhos não pagos: status → checkout (lembrete final)
```

### 3.2 Novos Status

| Status | Descricao | Checkout | Pode Adicionar Itens |
|--------|-----------|----------|---------------------|
| `active` | Live em andamento | ✅ Sim | ✅ Sim |
| `checkout` | Live finalizada, aguardando pagamento | ✅ Sim | ❌ Nao |
| `expired` | Expirou | ❌ Nao | ❌ Nao |
| `paid` | Pago | ❌ Nao | ❌ Nao |

### 3.3 Comportamento do Checkout Durante Live

1. **Carrinho pode ser atualizado enquanto no checkout**
   - Novos comentarios adicionam itens
   - Pagina de checkout reflete atualizacoes

2. **Pagamento congela carrinho**
   - Ao iniciar pagamento, carrinho entra em "lock"
   - Novos itens vao para carrinho separado ou fila

3. **Multiplos acessos ao mesmo link**
   - Token do carrinho permanece valido
   - Estado sempre atualizado

---

## 4. Mudancas Necessarias

### 4.1 Backend

#### 4.1.1 Mudanca no Status

```sql
-- Migration: alterar status permitidos
ALTER TABLE carts
  DROP CONSTRAINT IF EXISTS carts_status_check;

ALTER TABLE carts
  ADD CONSTRAINT carts_status_check
  CHECK (status IN ('active', 'checkout', 'expired', 'paid', 'processing'));

-- Atualizar carrinhos existentes
UPDATE carts SET status = 'active' WHERE status = 'pending';
```

#### 4.1.2 Mudanca no Service

```go
// checkout/service.go - ANTES
func (s *Service) GetCartForCheckout(ctx context.Context, input GetCartForCheckoutInput) (*CartForCheckoutResponse, error) {
    cart, err := s.repo.GetCartByToken(ctx, input.Token)
    if cart.Status == "pending" {
        return nil, httpx.ErrBadRequest("Carrinho ainda não está pronto")
    }
    // ...
}

// checkout/service.go - DEPOIS
func (s *Service) GetCartForCheckout(ctx context.Context, input GetCartForCheckoutInput) (*CartForCheckoutResponse, error) {
    cart, err := s.repo.GetCartByToken(ctx, input.Token)

    // Permitir checkout para status active OU checkout
    if cart.Status != "active" && cart.Status != "checkout" {
        if cart.Status == "expired" {
            return nil, httpx.ErrBadRequest("Este carrinho expirou")
        }
        if cart.Status == "paid" {
            return nil, httpx.ErrBadRequest("Este carrinho já foi pago")
        }
        return nil, httpx.ErrBadRequest("Carrinho não disponível para checkout")
    }
    // ...
}
```

#### 4.1.3 Criar Carrinho com Status Active

```go
// live/service.go ou onde carrinho é criado
func (s *Service) CreateOrUpdateCart(ctx context.Context, input CreateCartInput) (*Cart, error) {
    // Criar com status 'active' ao inves de 'pending'
    cart := &Cart{
        EventID:        input.EventID,
        PlatformUserID: input.PlatformUserID,
        PlatformHandle: input.PlatformHandle,
        Status:         "active", // MUDANCA: era "pending"
        Token:          generateToken(),
        ExpiresAt:      time.Now().Add(cartExpiration),
    }
    // ...
}
```

#### 4.1.4 Lock de Carrinho Durante Pagamento

```go
// checkout/service.go
func (s *Service) ProcessCardPayment(ctx context.Context, input ProcessCardPaymentInput) (*ProcessCardPaymentResponse, error) {
    // 1. Obter carrinho
    cart, err := s.repo.GetCartByToken(ctx, input.Token)

    // 2. Lock do carrinho (evitar modificacoes durante pagamento)
    if err := s.repo.LockCart(ctx, cart.ID); err != nil {
        return nil, err
    }
    defer s.repo.UnlockCart(ctx, cart.ID) // unlock em caso de falha

    // 3. Verificar se itens ainda estao disponiveis
    if err := s.validateCartItems(ctx, cart); err != nil {
        return nil, err
    }

    // 4. Processar pagamento
    // ...
}
```

```sql
-- Adicionar campo de lock
ALTER TABLE carts ADD COLUMN locked_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE carts ADD COLUMN locked_until TIMESTAMP WITH TIME ZONE;
```

### 4.2 Frontend

#### 4.2.1 Pagina de Checkout

```typescript
// cart/[token]/page.tsx - ANTES
if (cart.status !== "checkout") {
  return <ErrorState message="Carrinho ainda não está pronto" />
}

// cart/[token]/page.tsx - DEPOIS
if (cart.status !== "active" && cart.status !== "checkout") {
  if (cart.status === "expired") {
    return <ErrorState message="Este carrinho expirou" />
  }
  return <ErrorState message="Carrinho não disponível" />
}

// Mostrar indicador de "live em andamento"
{cart.status === "active" && (
  <Badge variant="outline" className="animate-pulse">
    🔴 Live em andamento - carrinho pode ser atualizado
  </Badge>
)}
```

#### 4.2.2 Polling para Atualizacoes

```typescript
// Quando carrinho está "active", fazer polling para atualizacoes
const { data: cart, refetch } = useQuery({
  queryKey: ['cart', token],
  queryFn: () => checkoutService.getCart(token),
  refetchInterval: cart?.status === 'active' ? 5000 : false, // 5s durante live
})

// Mostrar toast quando carrinho for atualizado
useEffect(() => {
  if (previousItemCount && cart.items.length > previousItemCount) {
    toast.info("Novo item adicionado ao carrinho!")
  }
}, [cart.items.length])
```

---

## 5. API Changes

### 5.1 Response Atualizado

```typescript
// GET /api/public/checkout/:token
interface CartForCheckoutResponse {
  id: string
  status: "active" | "checkout" | "expired" | "paid"
  isLiveActive: boolean // NOVO: indica se live ainda está rolando
  canAddItems: boolean  // NOVO: pode receber mais itens
  isLocked: boolean     // NOVO: está em processo de pagamento
  items: CartItem[]
  summary: CartSummary
  store: StoreInfo
  event: EventInfo
  // ...
}
```

### 5.2 Novo Endpoint (Opcional)

```
# Verificar se carrinho foi atualizado (lightweight)
GET /api/public/checkout/:token/status
Response: {
  itemCount: number
  total: number
  lastUpdatedAt: string
  isLocked: boolean
}
```

---

## 6. Casos de Uso

### 6.1 Fluxo Normal

1. Usuario comenta "ABC" na live
2. Carrinho criado com status `active`
3. Link de checkout enviado via DM
4. Usuario abre checkout, ve 1 item
5. Usuario comenta "XYZ" na live
6. Checkout atualiza automaticamente, agora 2 itens
7. Usuario finaliza pagamento
8. Carrinho status → `paid`

### 6.2 Pagamento Durante Atualizacao

1. Usuario tem carrinho com 2 itens
2. Usuario clica "Pagar"
3. Carrinho entra em LOCK
4. Usuario comenta mais um item na live
5. Item vai para FILA (ou novo carrinho)
6. Pagamento processado
7. Carrinho original → `paid`
8. Novo item fica disponivel para proximo checkout

### 6.3 Live Finalizada

1. Live termina
2. Carrinhos `active` → `checkout`
3. Usuarios recebem lembrete final
4. Comportamento igual ao atual

---

## 7. Implementacao

### 7.1 Steps Backend

- [ ] Migration para novo status e campos de lock
- [ ] Atualizar criacao de carrinho para status `active`
- [ ] Remover bloqueio de checkout para status `active`
- [ ] Implementar lock de carrinho durante pagamento
- [ ] Atualizar finalizacao de evento
- [ ] Adicionar campos `isLiveActive`, `canAddItems`, `isLocked` na response

### 7.2 Steps Frontend

- [ ] Aceitar status `active` na pagina de checkout
- [ ] Adicionar indicador de live em andamento
- [ ] Implementar polling para atualizacoes
- [ ] Mostrar feedback quando carrinho atualiza
- [ ] Desabilitar edicao quando carrinho locked

---

## 8. Testes

### 8.1 Unitarios

- [ ] Checkout permite status `active`
- [ ] Lock/unlock de carrinho
- [ ] Validacao de itens durante lock

### 8.2 Integracao

- [ ] Criar carrinho → checkout imediato
- [ ] Adicionar item durante checkout aberto
- [ ] Pagamento com lock

### 8.3 E2E

- [ ] Fluxo completo: comentario → checkout → pagamento durante live

---

## 9. Rollout

### 9.1 Estrategia

1. **Migracao de dados**
   - Carrinhos `pending` existentes → `active`

2. **Deploy backend**
   - Novo comportamento ativo

3. **Deploy frontend**
   - Nova UI com indicadores

### 9.2 Rollback

- Se problemas, reverter checkout para aceitar apenas `checkout`
- Dados permanecem compativeis

---

## 10. Metricas

| Metrica | Descricao |
|---------|-----------|
| `checkout_during_live_count` | Pagamentos feitos durante live |
| `checkout_after_live_count` | Pagamentos feitos apos live |
| `cart_updates_during_checkout` | Atualizacoes de carrinho com checkout aberto |
| `payment_lock_conflicts` | Conflitos de lock durante pagamento |

---

## 11. Checklist

- [ ] Migration criada e testada
- [ ] Backend atualizado
- [ ] Frontend atualizado
- [ ] Testes passando
- [ ] Documentacao atualizada
- [ ] Metricas configuradas
