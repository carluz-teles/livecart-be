DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM cart_erp_edits WHERE revision > synced_revision) THEN
        RAISE EXCEPTION 'Cannot remove pending ERP edits; reconcile them before rollback';
    END IF;
END $$;
DROP TABLE cart_erp_edit_requests;
DROP TABLE cart_erp_edits;
