package erp

import (
	"context"
	"fmt"
	"time"

	"livecart/apps/api/internal/cartedit"
)

type OrderRecoveryClaim struct {
	Operation StuckERPOrderOp
	Execution cartedit.Execution
	Pending   bool
	Release   func(error)
}

type OrderRecoveryClaimer interface {
	ClaimERPOrderRecovery(context.Context, string, time.Duration) (*OrderRecoveryClaim, error)
}

func (s *Service) recoverOrderOperation(ctx context.Context, op StuckERPOrderOp) (result error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if repo, ok := s.repo.(OrderRecoveryClaimer); ok {
		claim, err := repo.ClaimERPOrderRecovery(ctx, op.CartID, tempoDeOperacaoPresa)
		if err != nil {
			return err
		}
		if claim == nil {
			return nil
		}
		defer func() { claim.Release(result) }()
		op = claim.Operation
		if claim.Pending {
			if !claim.Execution.Remote {
				return fmt.Errorf("local edit cannot recover a remote operation: %w", cartedit.ErrReconciliation)
			}
			ctx = WithExpectedIntegration(ctx, claim.Execution.IntegrationID)
			active, err := s.activeERP(ctx, op.StoreID)
			if err != nil {
				return err
			}
			if active.Provider != claim.Execution.Provider {
				return fmt.Errorf("ERP provider changed after acceptance: %w", cartedit.ErrReconciliation)
			}
		}
	}
	switch {
	case op.State == OrderStateConverting && op.ExternalOrderID != "":
		return s.openCartOrder(ctx, op.StoreID, op.CartID, op.ExternalOrderID)
	case op.State == OrderStateConverting:
		return s.retomarCriacaoPresa(ctx, op.CartID, op.StoreID)
	case op.State == OrderStateReflecting:
		_, err := s.repo.TransitionCartERPOrderState(ctx, op.CartID, OrderStateReflecting, restingOrderState(op.RestingState))
		return err
	case op.State == OrderStateMutating && op.ExternalOrderID != "":
		if _, err := s.applyCartGridToOrder(ctx, op.CartID, op.StoreID, op.ExternalOrderID, nil); err != nil {
			return err
		}
		if _, err := s.repo.TransitionCartERPOrderState(ctx, op.CartID, OrderStateMutating, restingOrderState(op.RestingState)); err != nil {
			return err
		}
		s.collab.MirrorToOrder(ctx, op.CartID)
	}
	return nil
}
