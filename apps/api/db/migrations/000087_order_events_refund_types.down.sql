-- Volta ao enum original de 3 valores. Linhas com os tipos novos precisam ser
-- removidas antes, senao o CHECK antigo falha ao ser recriado.
DELETE FROM order_events WHERE event_type IN ('payment_cancelled', 'payment_refunded');

ALTER TABLE order_events DROP CONSTRAINT IF EXISTS order_events_event_type_check;

ALTER TABLE order_events
    ADD CONSTRAINT order_events_event_type_check
    CHECK (event_type IN (
        'payment_confirmed',
        'shipped',
        'delivered'
    ));
