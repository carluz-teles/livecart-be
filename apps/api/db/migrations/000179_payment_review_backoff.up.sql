ALTER TABLE erp_payment_reviews
    ADD COLUMN check_attempts INTEGER NOT NULL DEFAULT 0 CHECK (check_attempts BETWEEN 0 AND 6);
