-- O guard de sync/waitlist (HasStockGuardForProduct / HasInFlightFinalisationForProduct)
-- roda a cada webhook de estoque: precisa achar carts pagos com finalização
-- pendente por produto sem seq scan em cart_items.
CREATE INDEX IF NOT EXISTS idx_cart_items_product_id ON cart_items (product_id);
CREATE INDEX IF NOT EXISTS idx_carts_paid_erp_in_flight ON carts (payment_status, erp_finalisation_status)
    WHERE payment_status = 'paid' AND erp_finalisation_status <> 'done';
