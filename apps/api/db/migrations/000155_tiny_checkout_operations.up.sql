-- One compact recovery record per paid-order revision, not integration HTTP logs.
CREATE TABLE tiny_checkout_operations (
    id uuid PRIMARY KEY,
    cart_id uuid NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    integration_id uuid NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    source_order_id text NOT NULL CHECK (source_order_id <> ''),
    target_order_id text,
    progress jsonb NOT NULL CHECK (jsonb_typeof(progress) = 'object'),
    completed boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX tiny_checkout_operations_active_cart
    ON tiny_checkout_operations(cart_id) WHERE NOT completed;
CREATE INDEX tiny_checkout_operations_integration ON tiny_checkout_operations(integration_id);
CREATE INDEX tiny_checkout_operations_cart_history ON tiny_checkout_operations(cart_id,created_at DESC);
