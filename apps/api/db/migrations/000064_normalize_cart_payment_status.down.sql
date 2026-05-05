-- Revert the canonical default back to the legacy 'unpaid' sentinel. We do
-- NOT roll back the data update — once normalized to 'pending', leaving the
-- rows on 'pending' is harmless (existing queries already accept it) and
-- safer than re-introducing the dual-state problem.

ALTER TABLE carts ALTER COLUMN payment_status SET DEFAULT 'unpaid';
