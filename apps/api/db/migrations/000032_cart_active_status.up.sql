-- =============================================================================
-- CART STATUS: pending -> active
-- =============================================================================
-- Enable checkout during live by changing cart status from 'pending' to 'active'
-- Active status allows checkout while still accepting cart updates during live

-- Update existing pending carts to active
UPDATE carts SET status = 'active' WHERE status = 'pending';

-- Add comment for documentation
COMMENT ON COLUMN carts.status IS 'Cart status: active (live ongoing, checkout allowed), checkout (live ended), expired, paid';
