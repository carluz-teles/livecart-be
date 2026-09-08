package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

var ErrCommentBusy = errors.New("comment processing is already in progress")
var ErrCommentERPPending = errors.New("comment is waiting for ERP confirmation")

// CommentWorkRepository keeps retries independent of the delivery queue's retry
// budget. A lease coordinates webhook, polling and recovery workers.
type CommentWorkRepository interface {
	BeginCommentWork(context.Context, string, []byte) (owner string, completed bool, err error)
	FinishCommentWork(context.Context, string, string, error) error
	CommentWasAccepted(context.Context, string) (bool, error)
	AcceptComment(context.Context, string, []byte) error
	AcceptedCommentPlan(context.Context, string) ([]byte, error)
	ListPendingCommentWork(context.Context, int) ([][]byte, error)
}

func (s *Service) ProcessInstagramComment(ctx context.Context, input ProcessInstagramCommentInput) (err error) {
	work, persistent := s.ingestRepo.(CommentWorkRepository)
	if !persistent || input.CommentID == "" {
		return s.processInstagramComment(ctx, input)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	owner, completed, err := work.BeginCommentWork(ctx, input.CommentID, payload)
	if err != nil || completed {
		return err
	}
	// The operation expires before its lease can be claimed by another worker.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	defer func() {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer finishCancel()
		err = errors.Join(err, work.FinishCommentWork(finishCtx, input.CommentID, owner, err))
	}()
	accepted, err := work.CommentWasAccepted(ctx, input.CommentID)
	if err != nil {
		return err
	}
	if accepted {
		plan, err := work.AcceptedCommentPlan(ctx, input.CommentID)
		if err != nil {
			return err
		}
		ctx = context.WithValue(ctx, acceptedCommentKey{}, plan)
	}
	return s.processInstagramComment(ctx, input)
}

type acceptedCommentKey struct{}

func commentWasAccepted(ctx context.Context) bool {
	_, accepted := ctx.Value(acceptedCommentKey{}).([]byte)
	return accepted
}

// RecoverPendingComments is bounded so it can share the maintenance ticker.
// Every invocation claims its own lease; multiple replicas cannot process the
// same comment concurrently. Errors remain durable for the next invocation.
func (s *Service) RecoverPendingComments(ctx context.Context) {
	work, ok := s.ingestRepo.(CommentWorkRepository)
	if !ok {
		return
	}
	payloads, err := work.ListPendingCommentWork(ctx, 10)
	if err != nil {
		s.logger.Error("listing pending comments", zap.Error(err))
		return
	}
	started := time.Now()
	attempted, succeeded, deferred, busy, invalid := 0, 0, 0, 0, 0
	defer func() {
		if len(payloads) > 0 {
			s.logger.Info("comment recovery batch",
				zap.Int("selected", len(payloads)), zap.Int("attempted", attempted),
				zap.Int("succeeded", succeeded), zap.Int("deferred", deferred),
				zap.Int("busy", busy), zap.Int("invalid", invalid),
				zap.Duration("duration", time.Since(started)))
		}
	}()
	for _, payload := range payloads {
		if ctx.Err() != nil {
			return
		}
		var input ProcessInstagramCommentInput
		if err := json.Unmarshal(payload, &input); err != nil {
			invalid++
			s.logger.Error("decoding pending comment", zap.Error(err))
			continue
		}
		attempted++
		err := s.ProcessInstagramComment(ctx, input)
		switch {
		case errors.Is(err, ErrCommentBusy):
			busy++
		case err != nil:
			deferred++
			s.logger.Warn("comment remains pending", zap.String("comment_id", input.CommentID), zap.Error(err))
		default:
			succeeded++
			s.logger.Info("comment recovery completed", zap.String("comment_id", input.CommentID),
				zap.String("account_id", input.AccountID), zap.String("media_id", input.MediaID))
		}
	}
}

type acceptedPurchase struct {
	Item    PurchaseItem `json:"item"`
	Product *ProductRow  `json:"product"`
}

type acceptedCommentSnapshot struct {
	Event   *EventOutput       `json:"event"`
	Session *SessionOutput     `json:"session"`
	Items   []acceptedPurchase `json:"items"`
}

func commentSnapshot(ctx context.Context) (*acceptedCommentSnapshot, error) {
	raw, _ := ctx.Value(acceptedCommentKey{}).([]byte)
	var snapshot acceptedCommentSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.Event == nil || snapshot.Session == nil {
		return nil, fmt.Errorf("accepted comment has no session snapshot")
	}
	return &snapshot, nil
}

func acceptedPlan(ctx context.Context) ([]pedidoResolvido, error) {
	snapshot, err := commentSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	plan := snapshot.Items
	result := make([]pedidoResolvido, 0, len(plan))
	for _, item := range plan {
		result = append(result, pedidoResolvido{item: item.Item, product: item.Product})
	}
	return result, nil
}

func encodeAcceptedPlan(items []pedidoResolvido, event *EventOutput, session *SessionOutput) ([]byte, error) {
	plan := make([]acceptedPurchase, 0, len(items))
	for _, item := range items {
		plan = append(plan, acceptedPurchase{Item: item.item, Product: item.product})
	}
	return json.Marshal(acceptedCommentSnapshot{Event: event, Session: session, Items: plan})
}

type commentItemWriter interface {
	ApplyCommentItem(context.Context, AddToCartInput, string) (CommentItemResult, error)
}

type CommentItemResult struct {
	MaxQuantity int
	AddToCartOutput
	Quantity           int
	WaitlistedQuantity int
	AlreadyApplied     bool
	SkipReason         string
}

func (s *Service) applyPersistentCommentItem(ctx context.Context, writer commentItemWriter,
	event *EventOutput, session *SessionOutput, input ProcessInstagramCommentInput,
	commentID string, product *ProductRow, quantity int,
) (*resultadoDoItem, error) {
	result, err := writer.ApplyCommentItem(ctx, AddToCartInput{
		StoreID: event.StoreID, EventID: event.ID, SessionID: session.ID,
		PlatformUserID: input.UserID, PlatformHandle: input.Username,
		ProductID: product.ID, ProductPrice: product.Price, Quantity: quantity,
	}, input.CommentID)
	if err != nil {
		return nil, err
	}
	if result.Quantity == 0 && result.SkipReason == "max_quantity_reached" {
		s.sendMaxQuantityReply(ctx, event.StoreID, input.Channel, input.CommentID, input.UserID, input.Username, product.Name, result.MaxQuantity, true)
	}
	if result.Quantity == 0 {
		if commentID != "" {
			return nil, s.ingestRepo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, quantity, result.SkipReason)
		}
		return nil, nil
	}
	label := "added_to_cart"
	if result.WaitlistedQuantity > 0 {
		label = "waitlisted"
	}
	if commentID != "" {
		if err := s.ingestRepo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, result.Quantity, label); err != nil {
			return nil, err
		}
	}
	available := result.Quantity - result.WaitlistedQuantity
	pending := false
	if available > 0 && s.stockReserver != nil {
		if !result.AlreadyApplied {
			if err := s.stockReserver.NoteReserved(ctx, ReserveParams{Op: stockOpCartAdd,
				ProductID: product.ID, Quantity: available, CartID: result.CartID, EventID: event.ID}); err != nil {
				return nil, fmt.Errorf("recording stock reservation: %w", err)
			}
		}
		if err := s.stockReserver.ReserveStockInERP(ctx, event.StoreID, result.CartID, event.ID,
			product.ID, available, product.Price, input.Username); err != nil {
			pending = true
			s.logger.Warn("comment ERP synchronization remains pending", zap.String("cart_id", result.CartID), zap.Error(err))
		} else if err := s.ingestRepo.ConfirmarItemNoERP(ctx, result.CartID, product.ID); err != nil {
			return nil, err
		}
	}
	return &resultadoDoItem{carrinho: result.AddToCartOutput, produto: product,
		pedida: result.Quantity, naFila: result.WaitlistedQuantity, erpPendente: pending, replayed: result.AlreadyApplied}, nil
}
