DROP INDEX IF EXISTS idx_erp_stock_movements_reservation;
ALTER TABLE erp_stock_movements DROP COLUMN IF EXISTS reservation_id;
