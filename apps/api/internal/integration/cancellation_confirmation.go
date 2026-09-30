package integration

import "context"

func (s *Service) VerifyERPOrderCancelled(ctx context.Context, cartID, storeID string) error {
	return s.erpStock().VerifyERPOrderCancelled(ctx, cartID, storeID)
}
