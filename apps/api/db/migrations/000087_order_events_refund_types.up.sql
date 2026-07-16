-- order_events.event_type foi criado (000067) com um CHECK de 3 valores
-- (payment_confirmed, shipped, delivered), mas o fluxo pos-checkout passou a
-- emitir 'payment_cancelled' e 'payment_refunded' sem a migration prometida no
-- comentario da 000067. Resultado: todo INSERT desses tipos violava o CHECK e
-- os e-mails de cancelamento/estorno nunca eram enviados (o hook engolia o erro).
-- Estende o enum para cobrir os tipos que o codigo ja emite.

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
