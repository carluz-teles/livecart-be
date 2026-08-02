package live

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/logger"
)

// ProductRow is the enxuto product view the ingest flow matches a comment
// keyword against (id/keyword/price/stock/external-id/name). It lives here — not
// in integration — so the live package can own keyword matching without pulling
// in integration (cycle guard). integration.ProductRow is a type alias of this,
// so the repository keeps building the same value and every existing caller is
// unchanged.
type ProductRow struct {
	ID         string
	Keyword    string
	Price      int64
	Stock      int
	ExternalID string
	Name       string
}

// IngestRepository is the consumer-side persistence port the live ingest edge
// depends on. It is declared here (Go idiom: the interface lives with its
// consumer) and satisfied by internal/integration.Repository through a
// boot-wired adapter — live MUST NOT import integration/erp/inventory (cycle).
// It exposes exactly what the migrated ingest pieces need: the Catalog keyword
// lookup and the transactional outbox emit, with the tx encapsulated (no pool
// or queries leak across the port).
type IngestRepository interface {
	// GetProductByKeyword finds an active product by its 4-char keyword in a
	// store, or nil when there is no match.
	GetProductByKeyword(ctx context.Context, storeID, keyword string) (*ProductRow, error)
	// EmitCommentReceived writes the comment.received envelope to the outbox in
	// a single transaction (begin/emit/commit), so at-least-once redelivery is
	// deduped by the envelope DedupKey. The tx mechanics stay behind this port.
	EmitCommentReceived(ctx context.Context, env events.Envelope) error
}

// SetIngestRepository wires the ingest persistence port after construction. Like
// the other collaborators, it is injected via a setter (from integration, where
// the adapter over its Repository is built) so this package stays import-cycle
// free. Wired eagerly at boot, before any webhook is served.
func (s *Service) SetIngestRepository(r IngestRepository) { s.ingestRepo = r }

// DispatchCommentReceived is the inbound edge of the inverted comment flow: the
// webhook validates and dispatches a canonical comment.received event (via the
// transactional outbox) instead of processing inline. The domain work runs in
// the event consumer, which calls ProcessInstagramComment. The origin
// (live/story/dm) travels as the event Source, keeping the event canonical.
//
// Idempotency: ProcessInstagramComment already dedups by platform_comment_id, so
// at-least-once redelivery is safe. DedupKey mirrors that for visibility and the
// outbox unique constraint. payload carries the caller's input struct verbatim;
// its marshaled bytes are the event Payload, preserving the wire contract the
// consumer unmarshals back into.
func (s *Service) DispatchCommentReceived(ctx context.Context, commentID, mediaID string, source events.Source, payload any) error {
	if s.ingestRepo == nil {
		return fmt.Errorf("live: ingest repository not wired")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling comment.received payload: %w", err)
	}
	return s.ingestRepo.EmitCommentReceived(ctx, events.Envelope{
		Name:     events.CommentReceived,
		Source:   source,
		DedupKey: "comment.received:" + commentID,
		Metadata: map[string]string{"comment_id": commentID, "media_id": mediaID},
		Payload:  body,
	})
}

// FindProductByKeyword extracts possible keywords from text and returns the
// first active product that matches one, or nil when none match. A lookup error
// on one keyword is logged and skipped so a later keyword can still match.
func (s *Service) FindProductByKeyword(ctx context.Context, storeID, text string) *ProductRow {
	if s.ingestRepo == nil {
		return nil
	}
	keywords := ExtractPossibleKeywords(text)
	if len(keywords) == 0 {
		return nil
	}

	// Try each possible keyword until we find a match
	for _, keyword := range keywords {
		product, err := s.ingestRepo.GetProductByKeyword(ctx, storeID, keyword)
		if err != nil {
			logger.From(ctx, s.logger).Error("failed to lookup product by keyword",
				zap.String("keyword", keyword),
				zap.Error(err),
			)
			continue
		}
		if product != nil {
			return product
		}
	}

	return nil
}
