-- Change waitlisted from boolean to waitlisted_quantity integer
-- This allows partial fulfillment: some items available, some waitlisted

-- Step 1: Add new column
ALTER TABLE cart_items ADD COLUMN waitlisted_quantity INTEGER NOT NULL DEFAULT 0;

-- Step 2: Migrate existing data (if waitlisted=true, all quantity is waitlisted)
UPDATE cart_items SET waitlisted_quantity = quantity WHERE waitlisted = true;

-- Step 3: Drop old column
ALTER TABLE cart_items DROP COLUMN waitlisted;
