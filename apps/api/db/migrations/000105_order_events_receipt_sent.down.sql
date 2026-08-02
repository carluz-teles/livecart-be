-- Reverte a marca de recibo: remove as linhas 'receipt_sent' e volta o CHECK ao
-- conjunto da 000087 (sem 'receipt_sent').
DELETE FROM order_events WHERE event_type = 'receipt_sent';

ALTER TABLE order_events DROP CONSTRAINT IF EXISTS order_events_event_type_check;

ALTER TABLE order_events
    ADD CONSTRAINT order_events_event_type_check
    CHECK (event_type IN (
        'payment_confirmed',
        'payment_cancelled',
        'payment_refunded',
        'shipped',
        'delivered'
    ));
