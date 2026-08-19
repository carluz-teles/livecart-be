-- Liga cada movimento de estorno (direction 'in') à reserva que ele desfaz.
--
-- É o que permite a lei de conservação do razão: para cada reserva, exatamente
-- uma entrada confirmada quando 'reversed' — qualquer POST perdido ou dobrado
-- quebra a igualdade numa linha identificável. Nullable porque os movimentos de
-- saída (reserva) nascem antes de existir linha de reserva.
ALTER TABLE erp_stock_movements ADD COLUMN reservation_id UUID;

CREATE INDEX idx_erp_stock_movements_reservation
    ON erp_stock_movements(reservation_id)
    WHERE reservation_id IS NOT NULL;
