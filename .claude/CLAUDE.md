# CLAUDE-BACKEND.md — LiveCart API

## Como rodar os serviços

**IMPORTANTE: Sempre usar estes comandos para rodar os serviços.**

| Serviço | Diretório | Comando |
|---------|-----------|---------|
| **Backend** | `/home/carluz_teles/livecart-be` | `docker compose up` |
| **Frontend** | `/home/carluz_teles/livecart-fe` | `npm run dev` |

### Comandos úteis:

```bash
# Iniciar todos os serviços (API + DB + etc)
docker compose up

# Iniciar em background
docker compose up -d

# Rebuild da API após alterações no código
docker compose up -d --build api

# Ver logs da API
docker compose logs -f api

# Parar todos os serviços
docker compose down
```

### Notas:
- Backend API roda na porta **3001**
- Frontend roda na porta **3000**
- **Nunca** usar `go run` diretamente para o backend
- **Nunca** usar outras formas de iniciar o frontend além de `npm run dev`

---

## Stack

- **Linguagem**: Go 1.21+
- **Framework HTTP**: Fiber v2
- **Banco de dados**: PostgreSQL
- **ORM/Query Builder**: SQLC
- **Autenticação**: Clerk (JWT validation + SDK)
- **Container**: Docker + Docker Compose

---

## Estrutura do projeto

```
apps/api/
├── cmd/
│   └── http-server/
│       └── main.go          # Entry point
├── db/
│   ├── migrations/          # SQL migrations
│   └── sqlc/                # Generated SQLC code
├── internal/
│   ├── member/              # Member domain
│   ├── invitation/          # Invitation domain
│   ├── store/               # Store domain
│   └── ...
└── lib/
    ├── clerk/               # Clerk SDK wrapper
    ├── httpx/               # HTTP utilities
    └── valueobject/         # Value objects
```

---

## Convenções

### Camadas por domínio

Cada domínio (member, invitation, store, etc.) segue a estrutura:

- `handler.go` - HTTP handlers (Fiber)
- `service.go` - Business logic
- `repository.go` - Database access
- `types.go` - DTOs (Input/Output)
- `domain/` - Domain entities e value objects

### Tratamento de erros

Usar helpers do `lib/httpx`:
- `httpx.ErrNotFound("message")` → 404
- `httpx.ErrBadRequest("message")` → 400
- `httpx.ErrForbidden("message")` → 403
- `httpx.ErrUnprocessable("message")` → 422

### Logs

Usar `zap.Logger` injetado via construtor:
```go
s.logger.Info("action completed",
    zap.String("store_id", storeID),
    zap.String("user_id", userID),
)
```
