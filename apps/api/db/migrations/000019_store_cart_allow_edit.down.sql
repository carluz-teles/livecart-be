-- Remove cart_allow_edit setting from stores table
ALTER TABLE stores DROP COLUMN IF EXISTS cart_allow_edit;
