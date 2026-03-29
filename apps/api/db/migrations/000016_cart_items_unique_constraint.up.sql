-- Add unique constraint on cart_items for upsert behavior
ALTER TABLE cart_items
    ADD CONSTRAINT cart_items_cart_id_product_id_key UNIQUE (cart_id, product_id);
