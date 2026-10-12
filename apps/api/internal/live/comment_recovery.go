package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/erp"
)

var ErrCommentBusy = errors.New("comment processing is already in progress")
var ErrCommentERPBlocked = errors.New("comment requires ERP reconciliation: order closed for new items")
var ErrCommentERPPending = errors.New("comment is waiting for ERP confirmation")
var ErrCommentAwaitingBinding = errors.New("comment is waiting for live session binding")

const CommentBindingGracePeriod = 5 * time.Minute

// CommentBindingWork keeps the original delivery immutable across webhook,
// polling and retries. A binding deadline never extends with a new delivery.
type CommentBindingWork struct {
	Input     ProcessInstagramCommentInput
	WaitUntil *time.Time
	SessionID string
}

type CommentBindingRepository interface {
	LoadCommentBindingWork(context.Context, string, string) (CommentBindingWork, error)
	AwaitCommentBinding(context.Context, string, string) (bool, error)
	ResolveCommentBinding(context.Context, string, string) (string, error)
}

type commentBindingKey struct{}

type commentBindingContext struct {
	owner string
	work  CommentBindingWork
}

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

// ProcessInstagramCommentDelivery acknowledges redundant deliveries whose work
// is already durable and leased or scheduled for recovery. It must not mark that
// work completed: the current owner/recovery worker still owes the ERP result.
func (s *Service) ProcessInstagramCommentDelivery(ctx context.Context, input ProcessInstagramCommentInput) error {
	err := s.ProcessInstagramComment(ctx, input)
	// Only acknowledge these exact durable outcomes. A joined persistence error
	// must reach the delivery queue instead of being hidden by errors.Is.
	if err == ErrCommentBusy || err == ErrCommentAwaitingBinding {
		return nil
	}
	return err
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
		if finishErr := work.FinishCommentWork(finishCtx, input.CommentID, owner, err); finishErr != nil {
			err = errors.Join(err, finishErr)
		}
	}()
	var binding CommentBindingWork
	if bindings, ok := s.ingestRepo.(CommentBindingRepository); ok {
		binding, err = bindings.LoadCommentBindingWork(ctx, input.CommentID, owner)
		if err != nil {
			return err
		}
		input = binding.Input
	}
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
	} else if binding.WaitUntil != nil {
		// Check before looking up media: a binding that appears after the grace
		// period must not revive a purchase that was never accepted.
		if !time.Now().Before(*binding.WaitUntil) {
			s.logger.Info("comment binding grace expired", zap.String("comment_id", input.CommentID))
			return nil
		}
		bindings := s.ingestRepo.(CommentBindingRepository)
		binding.SessionID, err = bindings.ResolveCommentBinding(ctx, input.CommentID, owner)
		if err != nil {
			return err
		}
		if binding.SessionID == "" {
			return ErrCommentAwaitingBinding
		}
	}
	ctx = context.WithValue(ctx, commentBindingKey{}, commentBindingContext{owner: owner, work: binding})
	return s.processInstagramComment(ctx, input)
}

func (s *Service) awaitCommentBinding(ctx context.Context, input ProcessInstagramCommentInput) error {
	bindings, ok := s.ingestRepo.(CommentBindingRepository)
	state, leased := ctx.Value(commentBindingKey{}).(commentBindingContext)
	if !ok || !leased {
		return nil
	}
	waiting, err := bindings.AwaitCommentBinding(ctx, input.CommentID, state.owner)
	if err != nil {
		return err
	}
	if waiting {
		return ErrCommentAwaitingBinding
	}
	return nil
}

func commentBindingMatches(ctx context.Context, session *SessionOutput, event *EventOutput) bool {
	state, _ := ctx.Value(commentBindingKey{}).(commentBindingContext)
	if state.work.WaitUntil == nil || commentWasAccepted(ctx) {
		return true
	}
	return time.Now().Before(*state.work.WaitUntil) && session.ID == state.work.SessionID && session.EventID == event.ID
}

func recoveredCommentTime(ctx context.Context, input ProcessInstagramCommentInput) *time.Time {
	state, _ := ctx.Value(commentBindingKey{}).(commentBindingContext)
	if state.work.WaitUntil == nil || input.Timestamp <= 0 {
		return nil
	}
	createdAt := time.Unix(input.Timestamp, 0)
	return &createdAt
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
		case errors.Is(err, ErrCommentERPBlocked):
			deferred++
			s.logger.Info("comment awaits ERP reconciliation", zap.String("comment_id", input.CommentID))
		case err == ErrCommentAwaitingBinding:
			deferred++
			s.logger.Info("comment awaits live session binding", zap.String("comment_id", input.CommentID))
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
	ERPConfirmed       bool
	ERPBlocked         bool
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
	blocked := available > 0 && result.ERPBlocked && !result.ERPConfirmed
	if blocked {
		if err := s.ingestRepo.MarcarItemPendenteNoERP(ctx, result.CartID, product.ID); err != nil {
			return nil, err
		}
	}
	if available > 0 && s.stockReserver != nil && !result.ERPConfirmed && !blocked {
		if !result.AlreadyApplied {
			if err := s.stockReserver.NoteReserved(ctx, ReserveParams{Op: stockOpCartAdd,
				ProductID: product.ID, Quantity: available, CartID: result.CartID, EventID: event.ID}); err != nil {
				return nil, fmt.Errorf("recording stock reservation: %w", err)
			}
		}
		if err := s.stockReserver.ReserveStockInERP(ctx, event.StoreID, result.CartID, event.ID,
			product.ID, available, product.Price, input.Username); err != nil {
			pending = true
			blocked = errors.Is(err, erp.ErrPedidoFaturado)
			s.logger.Warn("comment ERP synchronization remains pending", zap.String("cart_id", result.CartID), zap.String("comment_id", input.CommentID), zap.String("product_id", product.ID), zap.Bool("requires_reconciliation", blocked), zap.Error(err))
		} else if err := s.ingestRepo.ConfirmarItemNoERP(ctx, result.CartID, product.ID); err != nil {
			return nil, err
		}
	}
	return &resultadoDoItem{carrinho: result.AddToCartOutput, produto: product,
		pedida: result.Quantity, naFila: result.WaitlistedQuantity, erpPendente: pending || blocked, erpBloqueado: blocked, replayed: result.AlreadyApplied}, nil
}
