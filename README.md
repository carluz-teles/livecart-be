# LiveCart API

API REST monolítica em Go para live commerce — detecção de pedidos via comentários, consolidação de carrinhos, integrações com ERPs e gateways de pagamento.

## Stack

- **Go** 1.26 + **Fiber** v2
- **PostgreSQL** 16 + PgBouncer (transaction mode)
- **sqlc** para geração de código a partir de SQL puro
- **golang-migrate** para migrations
- **zap** para logs estruturados (CloudWatch)
- **AWS SQS** para fila de eventos
- **AWS Lambda** para workers
- **Swagger** (swaggo) para documentação da API

## Requisitos

- Go 1.26+
- Docker e Docker Compose
- [sqlc](https://sqlc.dev/) (`go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`)
- [swag](https://github.com/swaggo/swag) (`go install github.com/swaggo/swag/cmd/swag@latest`)

## Quick Start

```bash
# Subir banco + API
docker compose up --build

# Subir banco + API + Tunnel (para integrações OAuth)
docker compose --profile dev up --build

# API disponível em http://localhost:3001
# Swagger UI em http://localhost:3001/swagger/
# Health check em http://localhost:3001/health
# Tunnel (com profile dev) em https://livecart-api.loca.lt
```

### Tunnel para Integrações

O tunnel expõe a API local para a internet, necessário para testar integrações OAuth (Mercado Pago, Tiny ERP).

```bash
# Iniciar com tunnel
docker compose --profile dev up

# Ver logs do tunnel
docker compose logs -f tunnel
```

**URLs de callback para configurar nas integrações:**
- Mercado Pago: `https://livecart-api.loca.lt/api/v1/integrations/oauth/mercado_pago/callback`
- Tiny ERP: `https://livecart-api.loca.lt/api/v1/integrations/oauth/tiny/callback`

## Desenvolvimento local (sem Docker)

```bash
# 1. Subir apenas o banco
docker compose up postgres -d

# 2. Configurar variáveis
export DATABASE_URL="postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable"
export APP_ENV=development
export PORT=3001

# 3. Rodar o servidor (migrations rodam automaticamente em dev)
go run ./apps/api/cmd/http-server
```

## Variáveis de Ambiente

| Variável | Descrição | Default |
|---|---|---|
| `DATABASE_URL` | Connection string PostgreSQL | — (obrigatória) |
| `APP_ENV` | `development` ou `production` | `development` |
| `PORT` | Porta do servidor HTTP | `3001` |
| `CLERK_SECRET_KEY` | Chave secreta do Clerk (JWT) | — |
| `AWS_REGION` | Região AWS | `us-east-1` |
| `AWS_SQS_QUEUE_URL` | URL da fila SQS | — |

## Estrutura do Projeto

```
apps/api/
├── cmd/
│   ├── http-server/main.go    # Entrypoint ECS — Fiber
│   └── worker/main.go         # Entrypoint Lambda — SQS consumer
├── internal/                   # Domínios (store, product, live, cart, order, integration, notification)
│   └── <domain>/
│       ├── handler.go          # Parse HTTP, validação de formato, serialização
│       ├── service.go          # Regras de negócio
│       ├── repository.go       # Acesso ao banco via sqlc
│       └── types.go            # Request/Response/Input/Output/Params/Row
├── lib/
│   ├── httpx/                  # Response helpers, errors, middlewares
│   ├── logger/                 # zap setup
│   ├── database/               # Pool pgx + migrations
│   ├── queue/                  # SQS client
│   └── clock/                  # Abstração de tempo
├── db/
│   ├── migrations/             # SQL migrations (golang-migrate)
│   ├── queries/                # SQL queries (sqlc)
│   └── sqlc/                   # Código gerado — NÃO editar
├── docs/                       # Swagger gerado — NÃO editar
└── sqlc.yaml
```

## Comandos Úteis

```bash
# Gerar código sqlc após alterar queries
cd apps/api && sqlc generate

# Gerar docs Swagger após alterar annotations
swag init -g apps/api/cmd/http-server/main.go -o apps/api/docs --parseDependency --parseInternal

# Build dos binários
go build ./apps/api/cmd/http-server
go build ./apps/api/cmd/worker

# Rodar testes
go test ./...
```

## Migrations

Migrations ficam em `apps/api/db/migrations/` no formato `NNNNNN_descricao.{up,down}.sql`.

- Em **dev**, migrations rodam automaticamente no startup
- Em **prod**, rodar como step separado no CI antes do deploy
- Nunca alterar uma migration já aplicada — sempre criar nova

```bash
# Criar nova migration
migrate create -ext sql -dir apps/api/db/migrations -seq descricao
```

## API Endpoints

### Public
| Método | Rota | Descrição |
|---|---|---|
| GET | `/health` | Health check |
| GET | `/swagger/*` | Swagger UI |

### Stores (autenticado)
| Método | Rota | Descrição |
|---|---|---|
| POST | `/api/v1/stores` | Criar store |
| GET | `/api/v1/stores/me` | Store atual |
| PUT | `/api/v1/stores/me` | Atualizar store |

### Products (autenticado)
| Método | Rota | Descrição |
|---|---|---|
| GET | `/api/v1/products` | Listar produtos |
| POST | `/api/v1/products` | Criar produto |
| GET | `/api/v1/products/:id` | Buscar produto |
| PUT | `/api/v1/products/:id` | Atualizar produto |

## Docker

```bash
# Build apenas do HTTP server
docker build --target http-server -t livecart-api .

# Build apenas do worker
docker build --target worker -t livecart-worker .
```
