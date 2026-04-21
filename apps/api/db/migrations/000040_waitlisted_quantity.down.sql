-- Revert waitlisted_quantity back to waitlisted boolean

-- Step 1: Add old column back
ALTER TABLE cart_items ADD COLUMN waitlisted BOOLEAN NOT NULL DEFAULT false;

-- Step 2: Migrate data (if any waitlisted_quantity > 0, mark as waitlisted)
UPDATE cart_items SET waitlisted = true WHERE waitlisted_quantity > 0;

-- Step 3: Drop new column
ALTER TABLE cart_items DROP COLUMN waitlisted_quantity;
