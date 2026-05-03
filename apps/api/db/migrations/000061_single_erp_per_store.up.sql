-- Enforce: at most one ERP integration per store. To switch ERPs the merchant
-- must disconnect the current one first.
--
-- Implemented as a partial unique index over store_id, scoped to type='erp'.
-- This leaves payment/shipping/social integrations free to coexist (multiple
-- payment providers per store is a real, supported case).
--
-- Safe to add today: only `tiny` is allowed by the existing provider check
-- constraint, so no merchant can have two ERPs in production.

CREATE UNIQUE INDEX uniq_integrations_store_one_erp
    ON integrations (store_id)
    WHERE type = 'erp';
