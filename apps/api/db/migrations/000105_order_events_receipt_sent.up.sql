-- Fatia A1: o recibo do comprador (SendOrderPaid) virou um reactor do dominio
-- Notification (notification/listeners.OnCartPaid -> postcheckout.SendPaidReceipt).
-- Sob asynq at-least-once ele precisa de uma marca de exatamente-uma-vez que NAO
-- colida com 'payment_confirmed' (esse e gravado antes, no OnCartPaid). Usa um
-- order_event dedicado 'receipt_sent', dedupado pelo indice unique
-- (order_id, event_type) ja existente. E um marcador INTERNO: a timeline publica
-- o filtra (postcheckout/handler.go). Estende o CHECK do event_type (mesma razao
-- da 000087, que adicionou os tipos de cancelamento/estorno).

ALTER TABLE order_events DROP CONSTRAINT IF EXISTS order_events_event_type_check;

ALTER TABLE order_events
    ADD CONSTRAINT order_events_event_type_check
    CHECK (event_type IN (
        'payment_confirmed',
        'payment_cancelled',
        'payment_refunded',
        'receipt_sent',
        'shipped',
        'delivered'
    ));
