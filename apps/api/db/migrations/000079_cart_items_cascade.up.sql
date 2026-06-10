-- Deleting an event cascades to its carts (carts.event_id ON DELETE CASCADE),
-- but cart_items.cart_id had no cascade — so deleting any event with carts
-- failed with a foreign-key violation (500). Items are owned by their cart;
-- cascade them. Every other carts dependent already cascades or sets null.
ALTER TABLE cart_items DROP CONSTRAINT IF EXISTS cart_items_cart_id_fkey;
ALTER TABLE cart_items
    ADD CONSTRAINT cart_items_cart_id_fkey
    FOREIGN KEY (cart_id) REFERENCES carts(id) ON DELETE CASCADE;
