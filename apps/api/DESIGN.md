# LiveCart API - Design Guidelines

Este documento define os padrões de design que **DEVEM** ser seguidos em todo o backend.

---

## 1. Arquitetura de Camadas

Cada módulo segue a arquitetura **Handler → Service → Repository**:

```
internal/
└── {module}/
    ├── handler.go      # HTTP handlers, validação de request, conversão de tipos
    ├── service.go      # Regras de negócio, orquestração, logging
    ├── repository.go   # Acesso a dados (SQLC queries)
    └── types.go        # Tipos separados por camada
```

### Responsabilidades por Camada

| Camada | Responsabilidade | NÃO Faz |
|--------|------------------|---------|
| **Handler** | Parse request, validação via tags, conversão Request→Input, retorno HTTP | Lógica de negócio, acesso a DB |
| **Service** | Regras de negócio, logging estruturado, orquestração | Parse HTTP, acesso direto a DB |
| **Repository** | Queries SQL, conversão DB→Row types | Lógica de negócio, HTTP |

---

## 2. Organização de Types (`types.go`)

Cada módulo tem **UM** arquivo `types.go` com tipos organizados por camada:

```go
package product

import "time"

// ============================================
// Handler layer - Request/Response types
// ============================================

type CreateProductRequest struct {
    Name  string `json:"name" validate:"required,min=1,max=200"`
    Price int64  `json:"price" validate:"min=0"`
}

type ProductResponse struct {
    ID        string    `json:"id"`
    Name      string    `json:"name"`
    CreatedAt time.Time `json:"createdAt"`
}

type ListProductsResponse struct {
    Data []ProductResponse `json:"data"`
}

// ============================================
// Service layer - Input/Output types
// ============================================

type CreateProductInput struct {
    StoreID string
    Name    string
    Price   int64
}

type ProductOutput struct {
    ID        string
    Name      string
    CreatedAt time.Time
}

// ============================================
// Repository layer - Params/Row types
// ============================================

type CreateProductParams struct {
    StoreID string
    Name    string
    Price   int64
}

type ProductRow struct {
    ID        string
    StoreID   string
    Name      string
    CreatedAt time.Time
}
```

### Convenções de Nomenclatura

| Camada | Sufixo Input | Sufixo Output |
|--------|--------------|---------------|
| Handler | `*Request` | `*Response` |
| Service | `*Input` | `*Output` |
| Repository | `*Params` | `*Row` |

---

## 3. Validação

### 3.1 Validação via Struct Tags (Handler Layer)

Toda validação de campos é feita via tags `validate` nos tipos `*Request`:

```go
type CreateInvitationRequest struct {
    Email string `json:"email" validate:"required,email"`
    Role  string `json:"role" validate:"required,oneof=admin member"`
}

type UpdateProductRequest struct {
    Name  string `json:"name" validate:"required,min=1,max=200"`
    Stock int    `json:"stock" validate:"min=0"`
}
```

### 3.2 Handler Chama o Validator

```go
func (h *Handler) Create(c *fiber.Ctx) error {
    var req CreateProductRequest
    if err := c.BodyParser(&req); err != nil {
        return httpx.BadRequest(c, "invalid request body")
    }
    if err := h.validate.Struct(req); err != nil {
        return httpx.ValidationError(c, err)
    }
    // ... continua
}
```

### 3.3 Service NÃO Duplica Validação

O service **NUNCA** valida campos que já têm tags de validação:

```go
// ❌ ERRADO - Duplica validação
func (s *Service) Create(input CreateInput) error {
    if input.Role != "admin" && input.Role != "member" {
        return errors.New("invalid role")
    }
}

// ✅ CORRETO - Service foca em regras de negócio
func (s *Service) Create(input CreateInput) error {
    // Verificar se já existe (regra de negócio)
    existing, _ := s.repo.GetByEmail(ctx, input.Email)
    if existing != nil {
        return httpx.ErrConflict("already exists")
    }
}
```

---

## 4. Error Handling

### 4.1 Service Errors (`lib/httpx/errors.go`)

Use os erros padronizados do `httpx`:

```go
// Erros disponíveis
httpx.ErrBadRequest(msg)    // 400
httpx.ErrNotFound(msg)      // 404
httpx.ErrForbidden(msg)     // 403
httpx.ErrConflict(msg)      // 409
httpx.ErrGone(msg)          // 410
httpx.ErrUnprocessable(msg) // 422
```

### 4.2 Custom Errors no Service

Para erros de domínio específicos, crie tipos de erro:

```go
// Em service.go
type SelfRemovalError struct{}
func (e *SelfRemovalError) Error() string {
    return "cannot remove yourself from the store"
}

type CannotRemoveOwnerError struct{}
func (e *CannotRemoveOwnerError) Error() string {
    return "cannot remove the store owner"
}
```

### 4.3 Handler Converte Erros

```go
func (h *Handler) Remove(c *fiber.Ctx) error {
    err := h.svc.Remove(ctx, storeID, memberID)
    if err != nil {
        var selfErr *SelfRemovalError
        var ownerErr *CannotRemoveOwnerError
        if errors.As(err, &selfErr) || errors.As(err, &ownerErr) {
            return httpx.HandleServiceError(c, httpx.ErrForbidden(err.Error()))
        }
        return httpx.HandleServiceError(c, err)
    }
    return httpx.NoContent(c)
}
```

---

## 5. HTTP Responses

### 5.1 Use os Helpers do `httpx`

**SEMPRE** use os helpers de response:

```go
// ✅ CORRETO
return httpx.OK(c, data)           // 200 + {"data": ...}
return httpx.Created(c, data)      // 201 + {"data": ...}
return httpx.NoContent(c)          // 204
return httpx.Deleted(c, id)        // 200 + {"data": {"id": "..."}}
return httpx.BadRequest(c, msg)    // 400 + {"error": "..."}
return httpx.ValidationError(c, err) // 422 + {"error": "...", "fields": {...}}
return httpx.HandleServiceError(c, err) // Converte ServiceError em HTTP

// ❌ ERRADO
return c.JSON(httpx.Envelope{Data: data})
return c.Status(200).JSON(...)
```

### 5.2 Envelope Padrão

Todas as responses seguem o envelope:

```json
// Sucesso
{"data": {...}}

// Erro
{"error": "mensagem de erro"}

// Erro de validação
{"error": "validation failed", "fields": {"Name": "required"}}
```

---

## 6. Logging / Observabilidade

### 6.1 Logger com Namespace

Todo service usa `logger.Named()`:

```go
func NewService(repo *Repository, logger *zap.Logger) *Service {
    return &Service{
        repo:   repo,
        logger: logger.Named("invitation"), // ✅ Namespace para filtrar
    }
}
```

### 6.2 Logs Estruturados em Operações Críticas

Log operações que modificam estado:

```go
func (s *Service) Create(ctx context.Context, input CreateInput) (*Output, error) {
    // ... lógica

    s.logger.Info("invitation created",
        zap.String("store_id", input.StoreID),
        zap.String("email", input.Email),
        zap.String("role", input.Role),
    )

    return output, nil
}

func (s *Service) Remove(ctx context.Context, storeID, memberID string) error {
    // ... lógica

    s.logger.Info("member removed from store",
        zap.String("store_id", storeID),
        zap.String("member_id", memberID),
        zap.String("removed_by", requestingUserID),
    )

    return nil
}
```

### 6.3 O Que Logar

| Operação | Logar? | Campos Mínimos |
|----------|--------|----------------|
| Create | ✅ Sim | store_id, identificador do recurso |
| Update | ✅ Sim | store_id, id, campos alterados |
| Delete | ✅ Sim | store_id, id, quem deletou |
| List/Get | ❌ Não | - |
| Erros inesperados | ✅ Sim (via HandleServiceError) | - |

---

## 7. Swagger Documentation

### 7.1 Toda Função Handler Tem Swagger

```go
// Create godoc
// @Summary      Create invitation
// @Description  Creates an invitation for a user to join the store
// @Tags         invitations
// @Accept       json
// @Produce      json
// @Param        storeId path string true "Store UUID"
// @Param        request body CreateInvitationRequest true "Invitation details"
// @Success      201 {object} httpx.Envelope{data=InvitationResponse}
// @Failure      400 {object} httpx.Envelope
// @Failure      409 {object} httpx.Envelope
// @Failure      422 {object} httpx.ValidationEnvelope
// @Router       /api/v1/stores/{storeId}/invitations [post]
// @Security     BearerAuth
func (h *Handler) Create(c *fiber.Ctx) error {
```

### 7.2 Padrão de Anotações

| Anotação | Obrigatório | Descrição |
|----------|-------------|-----------|
| `@Summary` | ✅ | Descrição curta (< 50 chars) |
| `@Description` | ✅ | Descrição detalhada |
| `@Tags` | ✅ | Nome do módulo (plural) |
| `@Accept` | Se recebe body | `json` |
| `@Produce` | ✅ | `json` |
| `@Param` | Se tem params | path, query, body |
| `@Success` | ✅ | Status + tipo de response |
| `@Failure` | ✅ | Todos os erros possíveis |
| `@Router` | ✅ | Path + método |
| `@Security` | Se autenticado | `BearerAuth` |

---

## 8. RBAC e Middleware

### 8.1 Middleware de Acesso à Store

Rotas store-scoped usam `StoreAccessMiddleware`:

```go
// Em main.go
storeScoped := api.Group("/stores/:storeId")
storeScoped.Use(httpx.StoreAccessMiddleware(userRepo))
```

Isso popula o context com:
- `store_id` - ID da store
- `store_user_id` - ID do store_user (para o usuário atual)
- `store_role` - Role do usuário na store (owner/admin/member)

### 8.2 Middleware de Role

Para rotas que exigem role específica:

```go
func (h *Handler) RegisterRoutes(r fiber.Router) {
    members := r.Group("/members")
    members.Get("/", h.List)  // Qualquer membro pode listar
    members.Patch("/:id/role", httpx.RequireRole(RoleOwner, RoleAdmin), h.UpdateRole)
    members.Delete("/:id", httpx.RequireRole(RoleOwner, RoleAdmin), h.Remove)
}
```

### 8.3 Acessando Context no Handler

```go
func (h *Handler) Create(c *fiber.Ctx) error {
    storeID := httpx.GetStoreID(c)           // string
    storeUserID := httpx.GetStoreUserID(c)   // string (store_user.id)
    role := httpx.GetStoreRole(c)            // string (owner/admin/member)
    clerkUserID := httpx.GetUserID(c)        // string (clerk user id)
    email := httpx.GetEmail(c)               // string
}
```

---

## 9. Repository Pattern

### 9.1 Usar SQLC Queries

```go
type Repository struct {
    q *sqlc.Queries
}

func NewRepository(q *sqlc.Queries) *Repository {
    return &Repository{q: q}
}

func (r *Repository) GetByID(ctx context.Context, id, storeID string) (*ProductRow, error) {
    uid, err := parseUUID(id)
    if err != nil {
        return nil, err
    }
    storeUID, err := parseUUID(storeID)
    if err != nil {
        return nil, err
    }

    row, err := r.q.GetProduct(ctx, sqlc.GetProductParams{
        ID:      uid,
        StoreID: storeUID,
    })
    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, httpx.ErrNotFound("product not found")
        }
        return nil, fmt.Errorf("getting product: %w", err)
    }

    return toProductRow(row), nil
}
```

### 9.2 Helper para UUID

Cada repository tem um helper local:

```go
func parseUUID(s string) (pgtype.UUID, error) {
    var uid pgtype.UUID
    if err := uid.Scan(s); err != nil {
        return uid, httpx.ErrUnprocessable("invalid uuid")
    }
    return uid, nil
}
```

---

## 10. Wiring no `main.go`

### 10.1 Padrão de Inicialização

```go
// Repository → Service → Handler
productRepo := product.NewRepository(queries)
productSvc := product.NewService(productRepo, log)
productHandler := product.NewHandler(productSvc, validate)
productHandler.RegisterRoutes(storeScoped)
```

### 10.2 Ordem de Registro

1. Middleware global (recover, requestid, cors, logger)
2. Rotas públicas (swagger, health, webhooks)
3. Rotas autenticadas não-store-scoped (`/api/v1/users/*`)
4. Rotas store-scoped (`/api/v1/stores/:storeId/*`)

---

## 11. Constantes e Enums

### 11.1 Definir no `types.go`

```go
// Roles
const (
    RoleOwner  = "owner"
    RoleAdmin  = "admin"
    RoleMember = "member"
)

// Statuses
const (
    StatusPending  = "pending"
    StatusAccepted = "accepted"
    StatusRevoked  = "revoked"
)
```

### 11.2 Usar nas Validate Tags

```go
type CreateRequest struct {
    Role string `json:"role" validate:"required,oneof=admin member"`
    // Nota: owner não é permitido criar via API
}
```

---

## 12. Checklist para Novo Módulo

Ao criar um novo módulo, verificar:

- [ ] `types.go` com tipos separados por camada (Request/Response, Input/Output, Params/Row)
- [ ] `repository.go` com SQLC queries e helper `parseUUID`
- [ ] `service.go` com `logger.Named("module")` e logs em operações críticas
- [ ] `handler.go` com:
  - [ ] Swagger em todas as funções
  - [ ] Validação via `h.validate.Struct(req)`
  - [ ] Uso de `httpx.OK()`, `httpx.Created()`, etc.
  - [ ] Erro handling via `httpx.HandleServiceError()`
- [ ] Wiring no `main.go`
- [ ] Queries SQL em `db/queries/`
- [ ] Executar `sqlc generate`
- [ ] Executar `swag init`

---

## 13. Domain-Driven Design (DDD)

### 13.1 Estrutura de Domínio

Cada módulo com lógica de negócio complexa tem uma pasta `domain/`:

```
internal/
└── {module}/
    ├── domain/
    │   ├── {entity}.go        # Entidade principal com regras de negócio
    │   ├── {value_object}.go  # Value Objects específicos do domínio
    │   └── status.go          # VOs de status/enum
    ├── handler.go
    ├── service.go
    ├── repository.go
    └── types.go
```

### 13.2 Value Objects Gerais (`lib/valueobject/`)

Value Objects compartilhados entre módulos ficam em `lib/valueobject/`:

```go
// lib/valueobject/email.go
type Email struct { value string }
func NewEmail(raw string) (Email, error)
func (e Email) String() string
func (e Email) Equals(other Email) bool

// lib/valueobject/id.go
type StoreID struct { ID }
type MemberID struct { ID }
type ProductID struct { ID }
type OrderID struct { ID }

// lib/valueobject/role.go
type Role struct { value string }
func (r Role) IsOwner() bool
func (r Role) CanManageMembers() bool

// lib/valueobject/money.go
type Money struct { cents int64 }
func (m Money) Add(other Money) Money
func (m Money) Multiply(qty int) Money
```

### 13.3 Value Objects de Domínio

VOs específicos de um domínio ficam em `{module}/domain/`:

```go
// internal/product/domain/keyword.go
type Keyword struct { value string }
func NewKeyword(value string) (Keyword, error)
func NextKeyword(current string) (Keyword, error)

// internal/product/domain/external_source.go
type ExternalSource struct { value string }
var ExternalSourceManual = ExternalSource{value: "manual"}
var ExternalSourceTiny = ExternalSource{value: "tiny"}

// internal/invitation/domain/token.go
type InvitationToken struct { value string }
func GenerateToken() (InvitationToken, error)

// internal/invitation/domain/status.go
type InvitationStatus struct { value string }
var StatusPending = InvitationStatus{value: "pending"}
var StatusAccepted = InvitationStatus{value: "accepted"}
```

### 13.4 Entidades de Domínio

Entidades encapsulam regras de negócio e estado:

```go
// internal/member/domain/member.go
type Member struct {
    id        vo.MemberID
    storeID   vo.StoreID
    email     vo.Email
    role      vo.Role
    status    MemberStatus
    // ... campos privados
}

// Factory function para criar nova entidade
func NewMember(storeID vo.StoreID, email vo.Email, role vo.Role) (*Member, error)

// Reconstruct para carregar do banco (sem validação)
func Reconstruct(id vo.MemberID, ...) *Member

// Getters imutáveis
func (m *Member) ID() vo.MemberID { return m.id }
func (m *Member) Role() vo.Role { return m.role }

// Regras de negócio
func (m *Member) CanBeRemovedBy(actor *Member) error {
    if m.IsOwner() {
        return ErrCannotRemoveOwner
    }
    if m.id.Equals(actor.id) {
        return ErrCannotRemoveSelf
    }
    if !actor.CanManageMembers() {
        return ErrInsufficientPermission
    }
    return nil
}

// Mudanças de estado
func (m *Member) ChangeRole(newRole vo.Role) error {
    if m.IsOwner() {
        return ErrCannotChangeOwnerRole
    }
    m.role = newRole
    m.updatedAt = time.Now()
    return nil
}
```

### 13.5 Repository Retorna Entidades

O repository converte rows do banco em entidades de domínio:

```go
func (r *Repository) GetByID(ctx context.Context, id vo.MemberID) (*domain.Member, error) {
    row, err := r.q.GetMember(ctx, id.ToPgUUID())
    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, httpx.ErrNotFound("member not found")
        }
        return nil, err
    }
    return toDomainMember(row)
}

func toDomainMember(row sqlc.StoreUser) (*domain.Member, error) {
    id, _ := vo.NewMemberID(row.ID.String())
    storeID, _ := vo.NewStoreID(row.StoreID.String())
    email, _ := vo.NewEmail(row.Email)
    role, _ := vo.NewRole(row.Role)
    status, _ := domain.NewMemberStatus(row.Status)

    return domain.Reconstruct(id, storeID, email, role, status, ...), nil
}
```

### 13.6 Service Usa Métodos de Domínio

O service orquestra e usa os métodos da entidade:

```go
func (s *Service) Remove(ctx context.Context, input RemoveMemberInput) error {
    // Busca entidades
    member, err := s.repo.GetByID(ctx, input.MemberID)
    if err != nil {
        return err
    }

    actor, err := s.repo.GetByID(ctx, input.ActorID)
    if err != nil {
        return err
    }

    // Usa método de domínio para validação
    if err := member.CanBeRemovedBy(actor); err != nil {
        return httpx.ErrForbidden(err.Error())
    }

    // Persiste
    return s.repo.Remove(ctx, member.ID())
}
```

### 13.7 Handler Converte para Value Objects

O handler converte strings em VOs antes de chamar o service:

```go
func (h *Handler) Remove(c *fiber.Ctx) error {
    storeIDStr := httpx.GetStoreID(c)
    memberIDStr := c.Params("memberId")
    actorIDStr := httpx.GetStoreUserID(c)

    // Converte para VOs
    storeID, err := vo.NewStoreID(storeIDStr)
    if err != nil {
        return httpx.BadRequest(c, "invalid store ID")
    }

    memberID, err := vo.NewMemberID(memberIDStr)
    if err != nil {
        return httpx.BadRequest(c, "invalid member ID")
    }

    actorID, err := vo.NewMemberID(actorIDStr)
    if err != nil {
        return httpx.BadRequest(c, "invalid actor ID")
    }

    err = h.svc.Remove(c.Context(), RemoveMemberInput{
        StoreID:  storeID,
        MemberID: memberID,
        ActorID:  actorID,
    })
    if err != nil {
        return httpx.HandleServiceError(c, err)
    }

    return httpx.NoContent(c)
}
```

### 13.8 Quando Usar DDD

Use DDD completo quando:
- O módulo tem regras de negócio complexas
- Há validações que dependem do estado da entidade
- Existem invariantes que precisam ser mantidas
- O domínio precisa de VOs específicos

Para módulos simples (CRUD básico), pode-se criar apenas os VOs de domínio sem migrar completamente o repository/service.

---

## 14. Comandos Úteis

```bash
# Gerar código SQLC
cd apps/api && sqlc generate

# Gerar Swagger
cd /livecart-be && swag init -g apps/api/cmd/http-server/main.go -o apps/api/docs --parseDependency --parseInternal

# Build
cd apps/api && go build ./...

# Run
cd apps/api && go run cmd/http-server/main.go
```
