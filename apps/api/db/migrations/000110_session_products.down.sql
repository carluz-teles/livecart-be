-- event_products permanece intacta, então o rollback só descarta a cópia.
-- O que NÃO volta: qualquer whitelist editada POR SESSÃO depois do backfill.
DROP TABLE IF EXISTS session_products;
