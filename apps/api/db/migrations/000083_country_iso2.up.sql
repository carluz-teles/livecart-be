-- Padroniza address_country como codigo ISO-2 (BR) — o FE gravava ora
-- "Brasil", ora "BR"; consultas e integracoes de frete esperam um formato so.
UPDATE stores SET address_country = 'BR'
WHERE address_country IS NULL
   OR TRIM(address_country) = ''
   OR LOWER(address_country) IN ('brasil', 'brazil', 'br');

ALTER TABLE stores ALTER COLUMN address_country SET DEFAULT 'BR';
