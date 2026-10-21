package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/live/domain"
	"livecart/apps/api/lib/httpx"
)

// This authorization is separate from accepted_plan: a merchant can authorize
// an unfulfilled comment, but cannot rewrite a purchase already accepted by ingest.
type manualCommentRecovery struct {
	CommentID      string                       `json:"commentId"`
	StoreID        string                       `json:"storeId"`
	EventID        string                       `json:"eventId"`
	SessionID      string                       `json:"sessionId"`
	CartID         string                       `json:"cartId"`
	ActorID        string                       `json:"actorId"`
	RequestedAt    time.Time                    `json:"requestedAt"`
	ReceivedAt     time.Time                    `json:"receivedAt"`
	OriginalResult string                       `json:"originalResult"`
	Product        *ProductRow                  `json:"product"`
	Quantity       int                          `json:"quantity"`
	Input          ProcessInstagramCommentInput `json:"input"`
}

type manualRecoveryKey struct{}

// CommentRecoveryRejected is terminal for this explicit authorization. It must
// never turn a late webhook into permission to retry a different purchase.
type CommentRecoveryRejected struct{ Reason string }

func (e *CommentRecoveryRejected) Error() string { return e.Reason }

func (s *Service) RecoverComment(ctx context.Context, input RecoverCommentInput) (*domain.CommentRecovery, error) {
	if s.repo == nil || s.ingestRepo == nil {
		return nil, httpx.DomainError(422, httpx.CodeCommentRecoveryInvalid, "recuperação de comentários indisponível")
	}
	if s.billingGate != nil && s.billingGate.IsStoreBlocked(ctx, input.StoreID) {
		return nil, httpx.DomainError(403, httpx.CodeCommentRecoveryForbidden, "loja bloqueada para novas compras")
	}
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var authorized bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memberships
		WHERE id=$1 AND store_id=$2 AND status='active' AND role IN ('owner','admin'))`, input.ActorID, input.StoreID).Scan(&authorized)
	if err != nil {
		return nil, err
	}
	if !authorized {
		return nil, httpx.DomainError(403, httpx.CodeCommentRecoveryForbidden, "somente owner/admin pode recuperar comentários")
	}
	m := manualCommentRecovery{
		CommentID: input.CommentID, StoreID: input.StoreID, EventID: input.EventID,
		ActorID: input.ActorID, RequestedAt: time.Now(), OriginalResult: "no_product",
	}
	var result string
	err = tx.QueryRow(ctx, `SELECT lc.platform_comment_id,lc.platform_user_id,lc.platform_handle,
		lc.text,COALESCE(lc.session_id::text,''),lc.created_at,COALESCE(lc.result,'')
		FROM live_comments lc JOIN live_events e ON e.id=lc.event_id
		WHERE lc.id=$1 AND lc.event_id=$2 AND e.store_id=$3 AND lc.deleted_at IS NULL`,
		input.CommentID, input.EventID, input.StoreID).
		Scan(&m.Input.CommentID, &m.Input.UserID, &m.Input.Username, &m.Input.Text, &m.SessionID, &m.ReceivedAt, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.DomainError(404, httpx.CodeCommentNotFound, "comentário não encontrado")
	}
	if err != nil {
		return nil, err
	}
	if m.Input.CommentID == "" || m.SessionID == "" {
		return nil, httpx.DomainError(409, httpx.CodeCommentRecoveryConflict, "comentário sem identidade ou transmissão original")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "comment:"+m.Input.CommentID); err != nil {
		return nil, err
	}
	// Historical records may not have a work row. Keep existing payloads intact.
	m.Input.Timestamp = m.ReceivedAt.Unix()
	payload, err := json.Marshal(m.Input)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO live_comment_work(platform_comment_id,payload,completed_at)
		VALUES($1,$2,now()) ON CONFLICT DO NOTHING`, m.Input.CommentID, string(payload)); err != nil {
		return nil, err
	}
	var accepted, completed, leased bool
	var previous, originalPayload []byte
	var rejection string
	err = tx.QueryRow(ctx, `SELECT accepted_at IS NOT NULL OR accepted_plan IS NOT NULL,
		completed_at IS NOT NULL,COALESCE(lease_until>now(),false),manual_recovery,payload,
		COALESCE(recovery_reason,'') FROM live_comment_work WHERE platform_comment_id=$1 FOR UPDATE`,
		m.Input.CommentID).Scan(&accepted, &completed, &leased, &previous, &originalPayload, &rejection)
	if err != nil {
		return nil, err
	}
	if len(previous) > 0 {
		if rejection != "" {
			return nil, httpx.DomainError(409, httpx.CodeCommentRecoveryConflict, rejection)
		}
		status := "already_queued"
		if completed {
			status = "already_processed"
		}
		return &domain.CommentRecovery{Status: status}, nil
	}
	// Refresh after acquiring the work lock; automatic ingest may have finished.
	if err := tx.QueryRow(ctx, `SELECT COALESCE(result,'') FROM live_comments WHERE id=$1 FOR UPDATE`,
		m.CommentID).Scan(&result); err != nil {
		return nil, err
	}
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cart_item_events WHERE platform_comment_id=$1)`,
		m.Input.CommentID).Scan(&applied); err != nil {
		return nil, err
	}
	if accepted || applied || leased || !completed || result != "no_product" {
		return nil, httpx.DomainError(409, httpx.CodeCommentRecoveryConflict, "somente comentários sem produto e sem compra aceita podem ser recuperados")
	}
	items := ParsePurchaseItems(m.Input.Text)
	if len(items) != 1 || items[0].Keyword == "" || items[0].Quantity < 1 {
		return nil, httpx.DomainError(422, httpx.CodeCommentRecoveryInvalid, "o comentário deve identificar um único código de produto e sua quantidade")
	}
	m.Quantity = items[0].Quantity
	storeID, err := parseUUID(m.StoreID)
	if err != nil {
		return nil, err
	}
	product, err := s.repo.q.WithTx(tx).GetProductByKeyword(ctx, sqlc.GetProductByKeywordParams{
		StoreID: storeID, Keyword: items[0].Keyword,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.DomainError(422, httpx.CodeCommentRecoveryInvalid, "produto não encontrado ou inativo; cadastre o código antes de recuperar")
	}
	if err != nil {
		return nil, err
	}
	m.Product = &ProductRow{ID: product.ID.String(), Keyword: product.Keyword, Name: product.Name,
		Price: product.Price.Int64, Stock: int(product.Stock.Int32), ExternalID: product.ExternalID.String}
	var original ProcessInstagramCommentInput
	if err := json.Unmarshal(originalPayload, &original); err != nil {
		return nil, fmt.Errorf("decoding original delivery: %w", err)
	}
	if original.Timestamp > 0 {
		m.ReceivedAt = time.Unix(original.Timestamp, 0)
	}
	if original.CommentID != m.Input.CommentID || original.UserID != m.Input.UserID || original.Text != m.Input.Text {
		return nil, httpx.DomainError(409, httpx.CodeCommentRecoveryConflict, "o registro original do comentário diverge da captura")
	}
	m.Input = original
	// A no_product may precede the buyer's first cart. Accept that sole purchase
	// in the original session, but never guess between purchases (even closed ones).
	err = tx.QueryRow(ctx, `SELECT c.id::text FROM carts c
		JOIN live_sessions s ON s.id=c.session_id AND s.event_id=c.event_id WHERE c.event_id=$1
		AND c.platform_user_id=$2 AND c.session_id=$3
		AND c.cancellation_reverted_at IS NULL
		AND c.created_at>=COALESCE(s.started_at,s.created_at)
		AND (s.ended_at IS NULL OR c.created_at<=s.ended_at)
		AND NOT EXISTS(SELECT 1 FROM carts other WHERE other.event_id=c.event_id
			AND other.platform_user_id=c.platform_user_id AND other.id<>c.id)`,
		m.EventID, m.Input.UserID, m.SessionID).Scan(&m.CartID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.DomainError(409, httpx.CodeCommentRecoveryConflict,
			"não há uma única compra do comprador na transmissão original; confira o pedido manualmente")
	}
	if err != nil {
		return nil, err
	}
	if err := validateManualRecovery(ctx, tx, &m); err != nil {
		return nil, err
	}
	if err := validateRecoveryCart(ctx, tx, &m); err != nil {
		return nil, err
	}
	authorization, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE live_comment_work SET manual_recovery=$2,
		completed_at=NULL,next_attempt_at=now(),last_error=NULL
		WHERE platform_comment_id=$1`, m.Input.CommentID, string(authorization)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE live_comments SET result='recovery_pending' WHERE id=$1`, m.CommentID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &domain.CommentRecovery{Status: "queued"}, nil
}

func validateManualRecovery(ctx context.Context, tx pgx.Tx, m *manualCommentRecovery) error {
	// The lock is short lived and contains no remote calls. It serializes block
	// insertion against admission, including handles with no prior block row.
	if _, err := tx.Exec(ctx, `LOCK TABLE blocked_handles IN SHARE MODE`); err != nil {
		return err
	}
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(e.status='active' AND e.ends_at>clock_timestamp()
		AND COALESCE(e.starts_at,e.scheduled_at,e.created_at)<=clock_timestamp()
		AND $4>=COALESCE(e.starts_at,e.scheduled_at,e.created_at) AND $4<e.ends_at
		AND $4::timestamptz<=clock_timestamp()
		AND $4>=COALESCE(s.started_at,s.created_at)
		AND (s.ended_at IS NULL OR $4<=s.ended_at)
		AND (s.status<>'ended' OR s.ended_at IS NOT NULL)
		AND NOT s.processing_paused
		AND EXISTS(SELECT 1 FROM live_comments lc WHERE lc.id=$6 AND lc.event_id=e.id
			AND lc.session_id=s.id AND lc.deleted_at IS NULL AND lc.platform_comment_id=$7
			AND lc.platform_user_id=$8 AND lc.text=$9)
		AND (NOT EXISTS(SELECT 1 FROM session_products sp WHERE sp.session_id=s.id)
			OR EXISTS(SELECT 1 FROM session_products sp WHERE sp.session_id=s.id AND sp.product_id=$10))
		AND NOT EXISTS(SELECT 1 FROM blocked_handles b WHERE b.store_id=e.store_id
			AND b.platform_handle=lower(ltrim(trim($5),'@')) AND b.unblocked_at IS NULL),false)
		FROM live_events e JOIN live_sessions s ON s.event_id=e.id
		WHERE e.id=$1 AND e.store_id=$2 AND s.id=$3 FOR SHARE OF e,s`,
		m.EventID, m.StoreID, m.SessionID, m.ReceivedAt, m.Input.Username,
		m.CommentID, m.Input.CommentID, m.Input.UserID, m.Input.Text, m.Product.ID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !allowed) {
		return httpx.DomainError(409, httpx.CodeCommentRecoveryConflict, "evento, transmissão ou perfil não permite recuperar este comentário")
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT active AND keyword=$3 FROM products WHERE id=$1 AND store_id=$2`,
		m.Product.ID, m.StoreID, m.Product.Keyword).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !allowed) {
		return httpx.DomainError(422, httpx.CodeCommentRecoveryInvalid, "o produto do código original não está mais disponível")
	}
	return err
}

func validateRecoveryCart(ctx context.Context, tx pgx.Tx, m *manualCommentRecovery) error {
	var ownerID string
	err := tx.QueryRow(ctx, `SELECT COALESCE(joined_to_cart_id,id)::text FROM carts
		WHERE id=$1 AND event_id=$2 AND platform_user_id=$3`, m.CartID, m.EventID, m.Input.UserID).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.DomainError(409, httpx.CodeCommentRecoveryConflict, "o carrinho original não está disponível")
	}
	if err != nil {
		return err
	}
	for _, id := range []string{ownerID, m.CartID} {
		var allowed bool
		err := tx.QueryRow(ctx, `SELECT COALESCE(COALESCE(c.store_id,e.store_id)=$2
			AND c.status IN ('active','checkout') AND NOT c.purchase_closed
			AND c.cancellation_reverted_at IS NULL AND c.session_id=$3
			AND EXISTS(SELECT 1 FROM live_sessions s WHERE s.id=$3 AND s.event_id=c.event_id
				AND c.created_at>=COALESCE(s.started_at,s.created_at)
				AND (s.ended_at IS NULL OR c.created_at<=s.ended_at))
			AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded')
			AND NOT c.payment_review_required AND erp_order_accepts_items(c.erp_order_status)
			AND (c.never_expires OR c.expires_at IS NULL OR c.expires_at>clock_timestamp()),false)
			FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1 FOR UPDATE OF c`, id, m.StoreID, m.SessionID).Scan(&allowed)
		if err != nil {
			return err
		}
		if !allowed {
			return httpx.DomainError(409, httpx.CodeCommentRecoveryConflict, "carrinho pago, expirado, cancelado, reaberto, fora da transmissão original ou fechado para novos itens")
		}
	}
	return cartedit.AssertReady(ctx, tx, ownerID)
}

func (r *Repository) loadManualRecovery(ctx context.Context, commentID, owner string) (*manualCommentRecovery, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx, `SELECT manual_recovery FROM live_comment_work
		WHERE platform_comment_id=$1 AND lease_owner=$2`, commentID, owner).Scan(&raw)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	var m manualCommentRecovery
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Service) processManualRecovery(ctx context.Context, m *manualCommentRecovery) error {
	if s.billingGate != nil && s.billingGate.IsStoreBlocked(ctx, m.StoreID) {
		return &CommentRecoveryRejected{Reason: "loja bloqueada para novas compras"}
	}
	tx, err := s.repo.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if err := validateManualRecovery(ctx, tx, m); err != nil {
		return recoveryProcessingError(err)
	}
	if err := validateRecoveryCart(ctx, tx, m); err != nil {
		return recoveryProcessingError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, manualRecoveryKey{}, m)
	item, err := s.applyPersistentCommentItem(ctx, s,
		&EventOutput{ID: m.EventID, StoreID: m.StoreID}, &SessionOutput{ID: m.SessionID},
		m.Input, "", m.Product, m.Quantity)
	if err != nil {
		return recoveryProcessingError(err)
	}
	result := "max_quantity_reached"
	quantity := 0
	if item != nil {
		result, quantity = "added_to_cart", item.pedida
		if item.naFila > 0 {
			result = "waitlisted"
		}
	}
	_, err = s.repo.pool.Exec(ctx, `UPDATE live_comment_work SET recovery_result=$2
		WHERE platform_comment_id=$1`, m.Input.CommentID, result)
	if err != nil {
		return err
	}
	if _, err := s.repo.pool.Exec(ctx, `UPDATE live_comments SET matched_product_id=$2,
		matched_quantity=$3 WHERE id=$1`, m.CommentID, m.Product.ID, quantity); err != nil {
		return err
	}
	if item != nil && item.erpBloqueado {
		return &CommentRecoveryRejected{Reason: "pedido exige conciliação com o ERP; recuperação interrompida"}
	}
	if item != nil && item.erpPendente {
		return ErrCommentERPPending
	}
	return nil
}

func recoveryProcessingError(err error) error {
	var domainError *httpx.ServiceError
	if errors.As(err, &domainError) {
		switch httpx.Code(domainError.Reason) {
		case httpx.CodeCommentRecoveryConflict, httpx.CodeCommentRecoveryInvalid, httpx.CodeCommentRecoveryForbidden:
			return &CommentRecoveryRejected{Reason: domainError.Message}
		}
	}
	return err
}
