package erp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

func (s *Service) syncPaidCheckout(ctx context.Context, provider providers.ERPProvider, cartID, storeID, externalOrderID string) (bool, error) {
	syncer, ok := provider.(providers.ERPOrderCheckoutSyncer)
	if !ok {
		return false, nil
	}
	loader, ok := s.collab.(interface {
		LoadERPOrderCheckout(context.Context, string, string) (providers.ERPOrderCheckout, error)
	})
	if !ok {
		return true, fmt.Errorf("paid checkout reader is not configured")
	}
	checkout, err := loader.LoadERPOrderCheckout(ctx, cartID, storeID)
	if err != nil {
		return true, err
	}
	err = s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		return syncer.SyncOrderCheckout(ctx, externalOrderID, checkout)
	})
	return true, err
}

// OnCartPaidBlingCheckout handles each cart.paid payment fact, whose dedup key
// contains the gateway payment ID. The first payment is still finalized by
// order.paid; later payments do not create a second immutable Order and must
// refresh the already confirmed Bling checkout through this separate hook.
func (s *Service) OnCartPaidBlingCheckout(ctx context.Context, cartID, storeID string) (resultErr error) {
	integration, err := s.repo.GetActiveERP(ctx, storeID)
	if errors.Is(err, pgx.ErrNoRows) || httpx.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading ERP for additional checkout payment: %w", err)
	}
	if integration.Provider != string(providers.ProviderBling) {
		return nil
	}
	ctx = logger.WithStore(ctx, storeID, "")
	state, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading paid checkout state: %w", err)
	}
	if state.State == OrderStateConverting || state.State == OrderStateMutating || state.State == OrderStateReflecting {
		return ErrOrderBusy
	}
	if state.State != OrderStateConfirmed {
		return nil
	}
	release, acquired, err := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if err != nil {
		return fmt.Errorf("locking additional checkout payment: %w", err)
	}
	if !acquired {
		return ErrOrderBusy
	}
	defer release()
	state, err = s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reloading paid checkout state: %w", err)
	}
	if state.State != OrderStateConfirmed {
		return ErrOrderBusy
	}
	if state.ExternalOrderID == "" {
		return ErrCartNotConverted
	}
	if closed, reason := pedidoJaFaturado(state.OrderStatus); closed {
		s.collab.MarkFinalisationFailed(ctx, cartID, "pagamento adicional exige reconciliação no ERP: "+reason)
		return fmt.Errorf("additional payment requires reconciliation: %s: %w", reason, ErrPedidoFaturado)
	}
	claimed, err := s.repo.TransitionCartERPOrderState(ctx, cartID, OrderStateConfirmed, OrderStateMutating)
	if err != nil {
		return fmt.Errorf("claiming additional checkout payment: %w", err)
	}
	if !claimed {
		return ErrOrderBusy
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		restored, err := s.repo.TransitionCartERPOrderState(cleanup, cartID, OrderStateMutating, OrderStateConfirmed)
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restoring paid checkout claim: %w", err))
		} else if !restored {
			resultErr = errors.Join(resultErr, ErrOrderBusy)
		}
		if resultErr != nil {
			s.collab.MarkFinalisationFailed(cleanup, cartID, "sincronização de pagamento adicional falhou: "+resultErr.Error())
		} else if err := s.repo.MarkCartERPFinalisationDone(cleanup, cartID); err != nil {
			resultErr = fmt.Errorf("recording additional checkout synchronization: %w", err)
			s.collab.MarkFinalisationFailed(cleanup, cartID, "falha ao registrar sincronização de pagamento adicional: "+err.Error())
		}
		s.collab.MirrorToOrder(cleanup, cartID)
	}()
	provider, err := s.collab.ResolveProvider(ctx, integration)
	if err != nil {
		return fmt.Errorf("resolving additional checkout provider: %w", err)
	}
	handled, err := s.syncPaidCheckout(ctx, provider, cartID, storeID, state.ExternalOrderID)
	if err != nil {
		return fmt.Errorf("synchronizing additional checkout payment: %w", err)
	}
	if !handled {
		return fmt.Errorf("Bling provider does not support paid checkout synchronization")
	}
	logger.From(ctx, s.logger).Info("confirmed Bling checkout reconciled after payment",
		zap.String("cart_id", cartID), zap.String("external_order_id", state.ExternalOrderID))
	return nil
}
