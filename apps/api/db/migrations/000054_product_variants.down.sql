DROP TABLE IF EXISTS product_group_images;
DROP TABLE IF EXISTS product_images;
DROP TABLE IF EXISTS product_variant_options;

ALTER TABLE products DROP COLUMN IF EXISTS group_id;

DROP TABLE IF EXISTS product_option_values;
DROP TABLE IF EXISTS product_options;
DROP TABLE IF EXISTS product_groups;
