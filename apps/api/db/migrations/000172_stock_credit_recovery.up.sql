-- A clamped mirror cannot safely accept relative credits until a fresh ERP
-- read replaces it, even after the blocked merchant edit is acknowledged.
ALTER TABLE erp_stock_sync_state
    ADD COLUMN credit_requires_refresh boolean NOT NULL DEFAULT false;
