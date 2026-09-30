package erp

import (
	"context"
	"errors"
	"fmt"

	"livecart/apps/api/internal/integration/providers"
)

var ErrCancellationUnconfirmed = errors.New("ERP cancellation requires reconciliation")

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
