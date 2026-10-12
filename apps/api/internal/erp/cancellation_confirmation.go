package erp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

var ErrCancellationUnconfirmed = errors.New("ERP cancellation requires reconciliation")

// CancellationTransition pins the local write to the purchase and account read
// before the remote operation. Payment webhooks do not take the ERP advisory lock.
type CancellationTransition struct {
	CartID          string
	StoreID         string
	IntegrationID   string
	ExternalOrderID string
	From            string
	To              string
	CartStatus      string
	OrderStatus     string
	ExpiresAt       *time.Time
	ExpiryOnly      bool
}

type cancellationRepository interface {
	TransitionCartCancellation(context.Context, CancellationTransition) (bool, error)
}

func cancellationProtected(st *CartERPOrderState, cartID string) bool {
	joined := st.CartID != "" && st.CartID != cartID
	paid := st.PaymentStatus == "paid" || st.PaymentStatus == "refunded" || st.PaidAmountCents > 0
	return joined || paid || st.PurchaseClosed
}

func expiredCancellationDue(st *CartERPOrderState) bool {
	return st.CartStatus == "expired" && st.ExpiresAt != nil && !st.ExpiresAt.After(time.Now())
}

func (s *Service) transitionCancellation(ctx context.Context, change CancellationTransition) (bool, error) {
	if repo, ok := s.repo.(cancellationRepository); ok {
		return repo.TransitionCartCancellation(ctx, change)
	}
	// In-memory adapters still recheck the binding; the production repository
	// performs this check and the update atomically under the row lock.
	st, err := s.repo.GetCartERPOrderState(ctx, change.CartID)
	if err != nil {
		return false, err
	}
	changed := st.ExternalOrderID != change.ExternalOrderID || st.CartStatus != change.CartStatus
	statusChanged := st.OrderStatus != change.OrderStatus && st.OrderStatus != string(providers.ERPOrderStatusCancelado)
	if changed || statusChanged || cancellationProtected(st, change.CartID) {
		return false, nil
	}
	return s.repo.TransitionCartERPOrderState(ctx, change.CartID, change.From, change.To)
}

func confirmRemoteCancellation(ctx context.Context, p providers.ERPProvider, orderID string) error {
	situation, err := p.GetOrderSituacao(ctx, orderID)
	if errors.Is(err, providers.ErrOrderNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading cancellation outcome: %w", err)
	}
	if situation != providers.SituacaoCancelada {
		return ErrCancellationUnconfirmed
	}
	return nil
}

// ReconcileExpiredCartCancellation only reads the ERP. It repairs an interrupted
// cancellation, including old expired carts, without cancelling a sale that a
// merchant has reopened or settled outside LiveCart.
func (s *Service) ReconcileExpiredCartCancellation(ctx context.Context, cartID, storeID string) error {
	release, acquired, err := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if err != nil {
		return fmt.Errorf("acquiring cancellation reconciliation lock: %w", err)
	}
	if !acquired {
		return ErrCartBusy
	}
	defer release()
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return err
	}
	if !expiredCancellationDue(st) || cancellationProtected(st, cartID) {
		return nil
	}
	if st.State != OrderStateOpen || st.OrderStatus != string(providers.ERPOrderStatusCancelado) {
		return nil
	}
	return s.reconcileCancelledBinding(ctx, cartID, storeID, st)
}

func (s *Service) reconcileCancelledBinding(
	ctx context.Context, cartID, storeID string, st *CartERPOrderState,
) error {
	if st.ExternalOrderID == "" {
		return ErrCancellationUnconfirmed
	}
	integration, err := s.activeERP(ctx, storeID)
	if err != nil {
		return err
	}
	p, err := s.collab.ResolveProvider(ctx, integration)
	if err != nil {
		return err
	}
	if err := confirmRemoteCancellation(ctx, p, st.ExternalOrderID); err != nil {
		return err
	}
	won, err := s.transitionCancellation(ctx, CancellationTransition{
		CartID: cartID, StoreID: storeID, IntegrationID: integration.ID,
		ExternalOrderID: st.ExternalOrderID, From: st.State, To: OrderStateCancelled,
		CartStatus: st.CartStatus, OrderStatus: st.OrderStatus, ExpiresAt: st.ExpiresAt, ExpiryOnly: true,
	})
	if err != nil {
		return fmt.Errorf("reconciling confirmed cancellation: %w", err)
	}
	if !won {
		return ErrCartBusy
	}
	s.collab.MirrorToOrder(ctx, cartID)
	return nil
}

// A local cancelled state is a claim, written BEFORE the remote cancellation.
// Only a GET against the original account can release an edit's retained stock.
func (s *Service) VerifyERPOrderCancelled(ctx context.Context, cartID, storeID string) error {
	release, acquired, err := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if err != nil {
		return err
	}
	if !acquired {
		return ErrCartBusy
	}
	defer release()
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return err
	}
	if st.CartID != "" && st.CartID != cartID {
		return fmt.Errorf("purchase owner changed: %w", ErrCancellationUnconfirmed)
	}
	if st.State != OrderStateCancelled && st.State != OrderStateNone {
		return ErrCartBusy
	}
	if st.ExternalOrderID == "" {
		// Creation can have delivered before its local binding was saved. An
		// empty ID, even after a crash, does not prove that no reservation exists.
		return fmt.Errorf("missing order binding: %w", ErrCancellationUnconfirmed)
	}
	p, err := s.providerFor(ctx, storeID)
	if err != nil {
		return err
	}
	return s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		situation, err := p.GetOrderSituacao(ctx, st.ExternalOrderID)
		if errors.Is(err, providers.ErrOrderNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if situation != providers.SituacaoCancelada {
			return fmt.Errorf("order remains active in ERP: %w", ErrCancellationUnconfirmed)
		}
		return nil
	})
}
