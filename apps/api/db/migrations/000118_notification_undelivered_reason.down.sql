-- Reverte a coluna e o indice. As linhas ja gravadas com status 'undelivered'
-- CONTINUAM la, sem motivo: o status e um valor livre em VARCHAR e nao ha
-- constraint que o rejeite. Perda de informacao aceita e declarada — o
-- rollback nao reescreve historico de envio.
DROP INDEX IF EXISTS idx_notification_logs_undelivered;

ALTER TABLE notification_logs
    DROP COLUMN IF EXISTS undelivered_reason;
