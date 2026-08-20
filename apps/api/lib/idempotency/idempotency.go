package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ErrDuplicateKey é devolvido por Repository.Create quando a chave já existe
// (violação de unique). Não é falha de infra: é o sinal de que OUTRA tentativa
// da mesma operação já passou por aqui — e Claim decide o que fazer com ela.
var ErrDuplicateKey = errors.New("idempotency key already exists")

// ErrInFlight indica que outra tentativa com a mesma chave está em andamento
// AGORA. Em 19/08/2026 este era o caso dos 2 stories: a primeira publicação
// ainda podia ter entrado no Instagram quando a segunda passou por cima.
var ErrInFlight = errors.New("an attempt with this idempotency key is already in flight")

// ErrPayloadMismatch indica que o cliente reusou uma chave de idempotência
// com um conteúdo diferente — bug do cliente, nunca um retry legítimo.
var ErrPayloadMismatch = errors.New("idempotency key reused with a different payload")

// Service handles idempotency checking and caching for integration operations.
type Service struct {
	repo Repository
}

// Repository defines the database operations needed for idempotency.
type Repository interface {
	GetByKey(ctx context.Context, storeID, key string) (*Record, error)
	GetByHash(ctx context.Context, storeID, hash string, windowStart time.Time) (*Record, error)
	// Create devolve ErrDuplicateKey (embrulhado) na violação de unique.
	Create(ctx context.Context, record CreateParams) (*Record, error)
	Update(ctx context.Context, id string, response []byte, status string) error
	// Reclaim toma posse (CAS) de um registro 'failed' — ou 'pending' velho o
	// bastante para a tentativa dona ter morrido — devolvendo false quando
	// outra tentativa chegou antes.
	Reclaim(ctx context.Context, id string) (bool, error)
}

// Record represents an idempotency record from the database.
type Record struct {
	ID             string
	IdempotencyKey string
	StoreID        string
	IntegrationID  string
	Operation      string
	RequestHash    string
	Response       []byte
	Status         string // pending, completed, failed
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// CreateParams contains parameters for creating an idempotency record.
type CreateParams struct {
	IdempotencyKey string
	StoreID        string
	IntegrationID  string
	Operation      string
	RequestHash    string
}

// NewService creates a new idempotency service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// CheckRequest contains the parameters for an idempotency check.
type CheckRequest struct {
	IdempotencyKey string // Explicit key from X-Idempotency-Key header
	StoreID        string
	IntegrationID  string
	Operation      string
	Payload        any
}

// CheckResult contains the result of an idempotency check.
type CheckResult struct {
	Found    bool
	Record   *Record
	Response []byte
}

// Check checks if a request is idempotent.
// Returns the cached response if found, nil otherwise.
func (s *Service) Check(ctx context.Context, req CheckRequest) (*CheckResult, error) {
	// First check explicit idempotency key
	if req.IdempotencyKey != "" {
		record, err := s.repo.GetByKey(ctx, req.StoreID, req.IdempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("checking idempotency key: %w", err)
		}
		if record != nil && record.Status == "completed" {
			return &CheckResult{
				Found:    true,
				Record:   record,
				Response: record.Response,
			}, nil
		}
		// If found but pending/failed, we'll proceed with the request
		// This allows retrying failed operations with the same key.
		//
		// When the client supplies an explicit key, NEVER fall back to the
		// payload-hash window: a fresh key is the client saying "this is a NEW
		// request". The hash intentionally ignores volatile fields (e.g. the
		// uploaded media URL), so two legitimately different requests can hash
		// alike — falling through here made a second Instagram post with the
		// same caption/products silently return the first one instead of
		// publishing.
		return &CheckResult{Found: false}, nil
	}

	// Fallback: check by payload hash within time window
	hash := s.hashPayload(req.Payload)
	windowStart := time.Now().Add(-5 * time.Minute) // 5-minute deduplication window

	record, err := s.repo.GetByHash(ctx, req.StoreID, hash, windowStart)
	if err != nil {
		return nil, fmt.Errorf("checking payload hash: %w", err)
	}
	if record != nil && record.Status == "completed" {
		return &CheckResult{
			Found:    true,
			Record:   record,
			Response: record.Response,
		}, nil
	}

	return &CheckResult{Found: false}, nil
}

// stalePendingAfter é a idade a partir da qual um registro 'pending' deixa de
// significar "em andamento" e passa a significar "a tentativa dona morreu no
// meio" (crash, deploy). Precisa casar com a janela do UPDATE de Reclaim.
const stalePendingAfter = 5 * time.Minute

// ClaimResult é o desfecho de Claim: exatamente um dos caminhos está ativo.
type ClaimResult struct {
	// Record é o registro pendente que ESTA tentativa possui (nil quando
	// Completed ou Unguarded).
	Record *Record
	// Completed é o registro de uma execução anterior que já terminou — o
	// chamador devolve a resposta dela em vez de executar de novo.
	Completed *Record
	// Reclaimed indica que Record foi tomado de uma tentativa anterior que
	// falhou (ou morreu); PriorResponse carrega o payload que ela deixou.
	Reclaimed     bool
	PriorResponse []byte
	// Unguarded: não há chave utilizável para travar (colisão da chave vazia
	// com outra operação) — o chamador segue sem registro, como antes.
	Unguarded bool
}

// Claim registra a intenção de executar a operação, com semântica de posse:
//
//   - ninguém passou por aqui → registro 'pending' novo;
//   - execução anterior COMPLETOU → devolve a resposta dela (Completed);
//   - tentativa anterior FALHOU (ou morreu 'pending' velha) → toma posse do
//     registro (Reclaimed) e entrega o payload da falha para o chamador
//     decidir COMO repetir — em publicação de mídia isso é a diferença entre
//     retomar o container antigo e criar um segundo story;
//   - tentativa anterior EM ANDAMENTO → ErrInFlight, nunca execução dupla.
//
// Substitui o Start antigo, cujo chamador tratava a violação de unique como
// "não consegui registrar" e executava assim mesmo — abrindo a trava
// exatamente no momento em que ela precisava segurar (19/08/2026).
func (s *Service) Claim(ctx context.Context, req CheckRequest) (*ClaimResult, error) {
	hash := s.hashPayload(req.Payload)

	record, err := s.repo.Create(ctx, CreateParams{
		IdempotencyKey: req.IdempotencyKey,
		StoreID:        req.StoreID,
		IntegrationID:  req.IntegrationID,
		Operation:      req.Operation,
		RequestHash:    hash,
	})
	if err == nil {
		return &ClaimResult{Record: record}, nil
	}
	if !errors.Is(err, ErrDuplicateKey) {
		return nil, fmt.Errorf("creating idempotency record: %w", err)
	}

	existing, gErr := s.repo.GetByKey(ctx, req.StoreID, req.IdempotencyKey)
	if gErr != nil {
		return nil, fmt.Errorf("loading existing idempotency record: %w", gErr)
	}
	if existing == nil {
		// A linha existe (o INSERT colidiu) mas a leitura não a enxerga —
		// registro expirado ainda não varrido. Sem posse possível.
		return nil, fmt.Errorf("idempotency record exists but is not visible (expired?)")
	}

	if existing.Status == "completed" {
		return &ClaimResult{Completed: existing}, nil
	}

	if existing.RequestHash != hash || existing.Operation != req.Operation {
		if req.IdempotencyKey == "" {
			// Colisão estrutural da chave vazia entre operações distintas da
			// mesma loja — não há o que travar; segue sem registro.
			return &ClaimResult{Unguarded: true}, nil
		}
		return nil, ErrPayloadMismatch
	}

	if existing.Status == "pending" && time.Since(existing.CreatedAt) < stalePendingAfter {
		return nil, ErrInFlight
	}

	ok, rErr := s.repo.Reclaim(ctx, existing.ID)
	if rErr != nil {
		return nil, fmt.Errorf("reclaiming idempotency record: %w", rErr)
	}
	if !ok {
		// CAS perdido: outra tentativa tomou posse entre a leitura e o UPDATE.
		return nil, ErrInFlight
	}

	prior := existing.Response
	reclaimed := *existing
	reclaimed.Status = "pending"
	reclaimed.Response = nil
	return &ClaimResult{Record: &reclaimed, Reclaimed: true, PriorResponse: prior}, nil
}

// Complete marks the idempotency record as completed with the response.
// Call this after the operation succeeds.
func (s *Service) Complete(ctx context.Context, id string, response any) error {
	respBytes, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("marshaling response: %w", err)
	}
	return s.repo.Update(ctx, id, respBytes, "completed")
}

// Fail marks the idempotency record as failed.
// Call this after the operation fails permanently.
func (s *Service) Fail(ctx context.Context, id string, opErr error) error {
	errResp, _ := json.Marshal(map[string]string{"error": opErr.Error()})
	return s.repo.Update(ctx, id, errResp, "failed")
}

// FailWithMeta marca a falha guardando, além do erro, o que a tentativa
// deixou para trás — em publicação de mídia, o container de desfecho
// desconhecido que o próximo Claim precisa retomar.
func (s *Service) FailWithMeta(ctx context.Context, id string, opErr error, meta map[string]any) error {
	payload := map[string]any{"error": opErr.Error()}
	for k, v := range meta {
		payload[k] = v
	}
	errResp, _ := json.Marshal(payload)
	return s.repo.Update(ctx, id, errResp, "failed")
}

// hashPayload creates a SHA-256 hash of the payload for deduplication.
func (s *Service) hashPayload(payload any) string {
	data, _ := json.Marshal(payload)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

// ParseUUID parses a UUID string into pgtype.UUID.
func ParseUUID(s string) (pgtype.UUID, error) {
	var uuid pgtype.UUID
	if err := uuid.Scan(s); err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid UUID: %s", s)
	}
	return uuid, nil
}

// UUIDToString converts pgtype.UUID to string.
func UUIDToString(uuid pgtype.UUID) string {
	if !uuid.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		uuid.Bytes[0:4],
		uuid.Bytes[4:6],
		uuid.Bytes[6:8],
		uuid.Bytes[8:10],
		uuid.Bytes[10:16])
}
