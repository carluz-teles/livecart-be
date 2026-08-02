-- Reversivel por inteiro: dropar a constraint nao toca em nenhuma linha.
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_store_id_fkey;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_event_id_fkey;
