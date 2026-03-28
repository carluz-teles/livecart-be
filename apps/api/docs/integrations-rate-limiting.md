# Rate Limiting para Integrações

## Visão Geral

O LiveCart usa um sistema de **rate limiting adaptativo** para proteger integrações com APIs externas (Tiny/Olist, MercadoPago, etc.). O sistema se auto-calibra usando os headers de rate limit retornados pela própria API, sem necessidade de configurar limites manualmente.

## Arquitetura

### Princípio: Nunca chegar no limite

Em vez de usar token bucket ou limites fixos, o sistema usa **throttling uniforme** baseado nos headers reais da API:

```
Chamada 1 (sem dados) → passa direto → API responde com headers
                                             ↓
                               X-RateLimit-Remaining: 55
                               X-RateLimit-Reset: 58 (segundos)
                                             ↓
                               Calcula intervalo: 58s / 55 = ~1.05s
                                             ↓
Chamada 2 → espera 1.05s → faz request → recebe novos headers → recalcula
```

### Componentes

```
┌─────────────────────────────────────────────────┐
│                  main.go                         │
│  rateLimitManager := ratelimit.NewManager(log)   │
└──────────────────┬──────────────────────────────┘
                   │
                   ▼
┌─────────────────────────────────────────────────┐
│              ratelimit.Manager                   │
│  Cache de AdaptiveLimiter por integration ID     │
│  GetOrCreate(integrationID) → *AdaptiveLimiter   │
└──────────────────┬──────────────────────────────┘
                   │
                   ▼
┌─────────────────────────────────────────────────┐
│           ratelimit.AdaptiveLimiter               │
│  Wait(ctx) — bloqueia até poder fazer request     │
│  UpdateFromHeaders(remaining, resetSeconds)       │
└──────────────────┬──────────────────────────────┘
                   │
                   ▼
┌─────────────────────────────────────────────────┐
│            providers.BaseProvider                 │
│  DoRequest():                                     │
│    1. RateLimiter.Wait(ctx)  ← throttling         │
│    2. HTTP request                                │
│    3. RateLimiter.UpdateFromHeaders() ← calibra   │
└─────────────────────────────────────────────────┘
```

### Pacote `lib/ratelimit/`

| Arquivo | Descrição |
|---------|-----------|
| `ratelimit.go` | Interfaces: `RateLimiter`, `Reservation`, `ErrRateLimited` |
| `adaptive.go` | `AdaptiveLimiter`: throttling uniforme via headers |
| `manager.go` | `Manager`: cache de limiters por integration ID |

## Como Funciona

### 1. Primeira chamada (sem dados)

Na primeira requisição, o limiter não tem dados da API. A chamada passa direto sem throttling. Isso é seguro porque uma única chamada nunca estoura o limite de nenhum provedor.

### 2. Throttling uniforme

Após a primeira resposta com headers, o limiter calcula o intervalo entre chamadas:

```
intervalo = tempoParaReset / remaining
```

**Exemplo** (plano Essencial Tiny, 120 req/min):
- API retorna: `Remaining: 100`, `Reset: 50s`
- Intervalo calculado: `50s / 100 = 0.5s` entre chamadas
- Chamadas são espaçadas uniformemente, nunca estourando o limite

### 3. Auto-calibração

Cada resposta da API atualiza o limiter com novos headers. Isso significa:
- Se outro app consumir da mesma conta, o `Remaining` diminui e o intervalo aumenta automaticamente
- Se a API mudar seus limites, o sistema se adapta sozinho
- Não é necessário saber o plano do usuário

### 4. Proteção contra 429

Se mesmo assim receber um 429 (Too Many Requests):
- `DoRequestWithRetry()` detecta o status 429
- Lê `X-RateLimit-Reset` ou `Retry-After` do header
- Espera o tempo indicado antes de retentativa
- O service layer marca a integração como `"error"` no dashboard

## Headers Utilizados

| Header | Descrição | Usado em |
|--------|-----------|----------|
| `X-RateLimit-Remaining` | Requisições restantes no ciclo | `UpdateFromHeaders()` |
| `X-RateLimit-Reset` | Segundos para reset do ciclo | `UpdateFromHeaders()` |
| `X-RateLimit-Limit` | Limite total (informativo) | Não usado diretamente |
| `Retry-After` | Segundos para retry após 429 | `DoRequestWithRetry()` |

## Limites por Provedor

### Tiny/Olist API v3

Limites **por conta** (todos os apps compartilham a cota):

| Plano | Req/min (total) | Escrita/min |
|-------|----------------|-------------|
| Básico / Crescer | 60 | 30 |
| Essencial / Evoluir | 120 | 60 |
| Grande / Potencializar | 240 | 100 |

- **429**: bloqueio temporário até próximo ciclo (API key NÃO é revogada)
- **Webhooks**: removidos após 20 falhas consecutivas (~15h50min)

### MercadoPago

Retorna headers `X-RateLimit-*` padrão. O sistema se adapta automaticamente.

## Tratamento de Erros

1. **Rate limit atingido** (`ErrRateLimited`): o service layer loga com nível `Error` e marca a integração como `"error"`, visível no dashboard do usuário.

2. **429 em retry**: `DoRequestWithRetry()` espera o tempo indicado pelo header e retenta.

3. **5xx**: backoff exponencial padrão (100ms, 200ms, 400ms... até 5s).

## Adicionando um Novo Provedor

1. Passe `RateLimiter` do config para `BaseProvider` no constructor (mesmo padrão do Tiny/MercadoPago)
2. Se a API retornar headers `X-RateLimit-*` → funciona automaticamente, zero config
3. Se usar headers diferentes (ex: `RateLimit-Remaining`) → adicione parsing em `DoRequest()` para esse padrão
4. Se NÃO retornar headers → o limiter permite tudo (sem throttling), e o tratamento de 429 serve como safety net
