-- =============================================================================
-- CART MUTATIONS — buyer-driven edits at checkout (audit log + dashboard reads)
-- =============================================================================

-- name: CreateCartMutation :one
INSERT INTO cart_mutations (
    cart_id, product_id, mutation_type,
    quantity_before, quantity_after, unit_price, source, erp_movement_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListCartMutations :many
SELECT
    cm.id,
    cm.cart_id,
    cm.product_id,
    cm.mutation_type,
    cm.quantity_before,
    cm.quantity_after,
    cm.unit_price,
    cm.source,
    cm.erp_movement_id,
    cm.created_at,
    p.name      AS product_name,
    p.image_url AS product_image_url,
    p.keyword   AS product_keyword
FROM cart_mutations cm
JOIN products p ON p.id = cm.product_id
WHERE cm.cart_id = $1
ORDER BY cm.created_at ASC;

-- name: AggregateCartMutations :many
-- Aggregates per-product net deltas across one cart for upsell/downsell card.
-- net_delta > 0 means the buyer added units, < 0 means removed/decreased.
SELECT
    cm.product_id,
    p.name      AS product_name,
    p.image_url AS product_image_url,
    p.keyword   AS product_keyword,
    SUM(CASE
        WHEN cm.mutation_type IN ('item_added','quantity_increased')
            THEN  (cm.quantity_after - cm.quantity_before)
        WHEN cm.mutation_type IN ('item_removed','quantity_decreased')
            THEN -(cm.quantity_before - cm.quantity_after)
        ELSE 0
    END)::int AS net_delta,
    MAX(cm.unit_price)::bigint AS unit_price
FROM cart_mutations cm
JOIN products p ON p.id = cm.product_id
WHERE cm.cart_id = $1 AND cm.source = 'buyer_checkout'
GROUP BY cm.product_id, p.name, p.image_url, p.keyword
HAVING SUM(CASE
        WHEN cm.mutation_type IN ('item_added','quantity_increased')
            THEN  (cm.quantity_after - cm.quantity_before)
        WHEN cm.mutation_type IN ('item_removed','quantity_decreased')
            THEN -(cm.quantity_before - cm.quantity_after)
        ELSE 0
    END) <> 0;

-- =============================================================================
-- CART INITIAL SNAPSHOT — frozen baseline taken on first checkout view
-- =============================================================================

-- name: EnsureCartInitialSnapshot :exec
-- Idempotently freezes the current cart_items into cart_initial_items and
-- stamps carts.initial_snapshot_taken_at + initial_subtotal_cents. Re-running
-- the query is a no-op once the snapshot is in place.
WITH should_snapshot AS (
    SELECT c.id FROM carts c
    WHERE c.id = $1 AND c.initial_snapshot_taken_at IS NULL
),
inserted AS (
    INSERT INTO cart_initial_items (cart_id, product_id, quantity, unit_price)
    SELECT ci.cart_id, ci.product_id,
           (ci.quantity - ci.waitlisted_quantity),
           ci.unit_price
    FROM cart_items ci
    JOIN should_snapshot s ON s.id = ci.cart_id
    WHERE ci.quantity - ci.waitlisted_quantity > 0
    ON CONFLICT (cart_id, product_id) DO NOTHING
    RETURNING cart_id, quantity, unit_price
)
UPDATE carts
SET initial_snapshot_taken_at = now(),
    initial_subtotal_cents = COALESCE((
        SELECT SUM(quantity * unit_price) FROM inserted
    ), 0)
WHERE carts.id IN (SELECT s.id FROM should_snapshot s);

-- name: ListCartInitialItems :many
SELECT
    cii.cart_id,
    cii.product_id,
    cii.quantity,
    cii.unit_price,
    p.name      AS product_name,
    p.image_url AS product_image_url,
    p.keyword   AS product_keyword
FROM cart_initial_items cii
JOIN products p ON p.id = cii.product_id
WHERE cii.cart_id = $1
ORDER BY p.name ASC;

-- name: GetCartInitialSummary :one
SELECT initial_snapshot_taken_at, initial_subtotal_cents
FROM carts
WHERE id = $1;

-- =============================================================================
-- DASHBOARD AGGREGATES (per event / per store)
-- =============================================================================

-- name: GetCheckoutUpsellMetricsByEvent :one
-- Aggregated upsell/downsell numbers for one event. Counts only paid carts to
-- avoid mixing intent (still in checkout) with realized revenue.
WITH per_cart AS (
    SELECT
        c.id AS cart_id,
        COALESCE(c.initial_subtotal_cents, 0) AS initial_cents,
        COALESCE((
            SELECT SUM((ci.quantity - ci.waitlisted_quantity) * ci.unit_price)
            FROM cart_items ci
            WHERE ci.cart_id = c.id AND ci.quantity > ci.waitlisted_quantity
        ), 0)::bigint AS final_cents,
        EXISTS (SELECT 1 FROM cart_mutations m WHERE m.cart_id = c.id AND m.source = 'buyer_checkout') AS has_mut
    FROM carts c
    WHERE c.event_id = $1 AND c.payment_status = 'paid'
)
SELECT
    COUNT(*) FILTER (WHERE has_mut)::int AS carts_with_mutations,
    COALESCE(SUM(GREATEST(final_cents - initial_cents, 0)), 0)::bigint AS upsell_cents,
    COALESCE(SUM(GREATEST(initial_cents - final_cents, 0)), 0)::bigint AS downsell_cents,
    COUNT(*)::int AS total_paid_carts
FROM per_cart;

-- name: ListTopUpsoldProductsByEvent :many
SELECT
    cm.product_id,
    p.name      AS product_name,
    p.image_url AS product_image_url,
    SUM(cm.quantity_after - cm.quantity_before)::int AS units_added,
    SUM((cm.quantity_after - cm.quantity_before) * cm.unit_price)::bigint AS revenue_cents
FROM cart_mutations cm
JOIN carts c ON c.id = cm.cart_id
JOIN products p ON p.id = cm.product_id
WHERE c.event_id = $1
  AND c.payment_status = 'paid'
  AND cm.source = 'buyer_checkout'
  AND cm.mutation_type IN ('item_added','quantity_increased')
GROUP BY cm.product_id, p.name, p.image_url
ORDER BY units_added DESC
LIMIT $2;

-- name: ListTopRemovedProductsByEvent :many
SELECT
    cm.product_id,
    p.name      AS product_name,
    p.image_url AS product_image_url,
    SUM(cm.quantity_before - cm.quantity_after)::int AS units_removed,
    SUM((cm.quantity_before - cm.quantity_after) * cm.unit_price)::bigint AS revenue_cents_lost
FROM cart_mutations cm
JOIN carts c ON c.id = cm.cart_id
JOIN products p ON p.id = cm.product_id
WHERE c.event_id = $1
  AND c.payment_status = 'paid'
  AND cm.source = 'buyer_checkout'
  AND cm.mutation_type IN ('item_removed','quantity_decreased')
GROUP BY cm.product_id, p.name, p.image_url
ORDER BY units_removed DESC
LIMIT $2;

-- =============================================================================
-- ACTIVE CHECKOUTS (live merchant view: carts in checkout phase right now)
-- =============================================================================

-- name: ListActiveCheckoutsByEvent :many
-- Carts the buyer is currently editing/paying for. Used by the live merchant
-- panel to show real-time mutation activity before the payment lands.
SELECT
    c.id,
    c.platform_handle,
    c.token,
    c.status,
    c.payment_status,
    c.created_at,
    c.expires_at,
    COALESCE(c.initial_subtotal_cents, 0)::bigint AS initial_subtotal_cents,
    COALESCE((
        SELECT SUM((ci.quantity - ci.waitlisted_quantity) * ci.unit_price)
        FROM cart_items ci
        WHERE ci.cart_id = c.id AND ci.quantity > ci.waitlisted_quantity
    ), 0)::bigint AS current_subtotal_cents,
    COALESCE((
        SELECT COUNT(*) FROM cart_mutations m WHERE m.cart_id = c.id AND m.source = 'buyer_checkout'
    ), 0)::int AS mutation_count,
    (SELECT MAX(m.created_at) FROM cart_mutations m WHERE m.cart_id = c.id AND m.source = 'buyer_checkout')::timestamptz AS last_mutation_at
FROM carts c
WHERE c.event_id = $1
  AND c.status = 'checkout'
  AND (c.payment_status IS NULL OR c.payment_status IN ('unpaid','pending','processing'))
ORDER BY c.created_at DESC;

-- =============================================================================
-- ORDER UPSELL — single order detail card
-- =============================================================================

-- name: GetOrderUpsellSummary :one
-- Summary numbers for one order's upsell card: initial vs final subtotals
-- and a mutation count for visibility. The full mutation list is fetched
-- separately via ListCartMutations.
SELECT
    COALESCE(c.initial_subtotal_cents, 0)::bigint AS initial_subtotal_cents,
    c.initial_snapshot_taken_at,
    COALESCE((
        SELECT SUM((ci.quantity - ci.waitlisted_quantity) * ci.unit_price)
        FROM cart_items ci
        WHERE ci.cart_id = c.id AND ci.quantity > ci.waitlisted_quantity
    ), 0)::bigint AS final_subtotal_cents,
    COALESCE((
        SELECT COUNT(*) FROM cart_mutations m WHERE m.cart_id = c.id AND m.source = 'buyer_checkout'
    ), 0)::int AS mutation_count
FROM carts c
WHERE c.id = $1;
