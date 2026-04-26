ALTER TABLE stores DROP CONSTRAINT IF EXISTS stores_default_height_cm_positive;
ALTER TABLE stores DROP CONSTRAINT IF EXISTS stores_default_width_cm_positive;
ALTER TABLE stores DROP CONSTRAINT IF EXISTS stores_default_length_cm_positive;

ALTER TABLE stores DROP COLUMN IF EXISTS default_height_cm;
ALTER TABLE stores DROP COLUMN IF EXISTS default_width_cm;
ALTER TABLE stores DROP COLUMN IF EXISTS default_length_cm;
