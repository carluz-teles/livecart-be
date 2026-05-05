-- Normalize cart payment_status so the merchant order list (which filters by
-- 'pending', 'paid', 'failed', 'refunded') always sees newly-created carts.
--
-- The original schema (000001) defaulted the column to 'unpaid' and never
-- migrated those rows when the payment vocabulary was canonicalized. Result:
-- carts created during a live ended up with payment_status='unpaid' and
-- vanished from every tab on /orders, since no tab filter includes 'unpaid'.
--
-- Existing 'unpaid', '' and NULL rows collapse into 'pending'. The column
-- default also moves to 'pending' so new carts inherit the canonical value
-- without depending on the inserter to specify it.

UPDATE carts
SET payment_status = 'pending'
WHERE payment_status IS NULL
   OR payment_status = ''
   OR payment_status = 'unpaid';

ALTER TABLE carts ALTER COLUMN payment_status SET DEFAULT 'pending';
