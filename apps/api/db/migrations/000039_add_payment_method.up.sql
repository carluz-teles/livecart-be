-- Add payment_method column to carts table
-- Possible values: 'pix', 'credit_card', 'debit_card', 'boleto', 'other'
ALTER TABLE carts ADD COLUMN payment_method VARCHAR DEFAULT NULL;

-- Create index for analytics queries
CREATE INDEX idx_carts_payment_method ON carts(payment_method) WHERE payment_method IS NOT NULL;
