DROP INDEX tiny_checkout_operations_recovery;
ALTER TABLE tiny_checkout_operations DROP COLUMN recovery_after, DROP COLUMN recovery_attempts;
DROP VIEW active_cart_erp_item_blocks;
DROP TABLE cart_erp_item_blocks;
