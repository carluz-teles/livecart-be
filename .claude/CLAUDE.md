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

### ⛔ ANTES de implementar QUALQUER coisa — checar se já existe (OBRIGATÓRIO)

**Regra nº 1, sem exceção:** antes de escrever qualquer código novo (query, função,
helper, cálculo, componente, endpoint), **verificar se já existe algo que faz aquilo**
e **reusar / centralizar** em vez de duplicar. Duplicar lógica = múltiplas fontes da
verdade que divergem (ex.: a fórmula do GMV `SUM(quantity*unit_price)` estava
reimplementada em ~20 lugares).

Checklist antes de começar:
1. **Buscar pela responsabilidade/fórmula**, não só pelo nome: `grep`/busca por
   colunas, tabelas, cálculos e conceitos envolvidos (ex.: antes de somar itens de
   um cart, `grep -rn "SUM(.*unit_price" db/queries`).
2. Se já existe → **reusar** (ou extrair para um ponto único e fazer todos apontarem
   pra lá). Se está duplicado → é bug de design; centralizar (SOLID/DRY, um ponto de manutenção).
3. Só criar algo novo depois de confirmar que **não há** equivalente.

Isto vale para agents de implementação (dev-qa, etc.) também — é a primeira etapa
de qualquer tarefa, antes de escrever teste ou código.

### Camadas por domínio

Cada domínio (member, invitation, store, etc.) segue a estrutura:

- `handler.go` - HTTP handlers (Fiber)
- `service.go` - Business logic
- `repository.go` - Database access
- `types.go` - DTOs (Input/Output)
- `domain/` - Domain entities e value objects

**Ao criar/alterar um domínio HTTP (handler, endpoint, Request DTO ou validação),
seguir a skill `api-domain-convention`** — fluxo `Request→Validate→ToInput→Usecase→Response`,
as três camadas de validação e o contrato de erro do httpx. Molde de referência: `internal/member`.

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

---

## Trabalhando no FRONTEND (livecart-fe) a partir desta sessao

**OBRIGATORIO**: antes de escrever qualquer codigo em `/home/carluz_teles/livecart-fe`,
ler e seguir `/home/carluz_teles/livecart-fe/.claude/CLAUDE.md`. Regras principais:

- **Camadas: Service → Hook → UI.** Component APENAS renderiza (zero logica de
  negocio); Hook gerencia estado/efeitos/regras e chama o service; Service faz
  a comunicacao com APIs (internas ou externas). Component nunca chama service.
- Server state SEMPRE via React Query (nunca useState+useEffect para fetch).
- Hooks por dominio em `src/hooks/{dominio}/`; services em `src/services/`.
- `npm run build` obrigatorio antes de push no FE.
