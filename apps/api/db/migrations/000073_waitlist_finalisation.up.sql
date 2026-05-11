-- TTL extra ("gordura") que o cliente notificado tem para finalizar o
-- carrinho depois que o produto da fila é liberado para ele. Configurável
-- por evento — alguns sellers preferem janelas curtas (live ao vivo) ou
-- longas (notificação por DM/email que pode demorar a ser aberta).
ALTER TABLE live_events
    ADD COLUMN IF NOT EXISTS waitlist_notified_ttl_minutes INT NOT NULL DEFAULT 30
        CHECK (waitlist_notified_ttl_minutes BETWEEN 5 AND 240);

COMMENT ON COLUMN live_events.waitlist_notified_ttl_minutes IS 'Minutos extras que um cliente promovido da waitlist (status=notified) tem para finalizar o checkout antes de devolver o estoque para o próximo da fila.';

-- Vincula a entrada da fila ao carrinho do cliente naquele evento, para
-- conseguirmos exibir os waitlist items no /cart/:token sem precisar
-- cruzar por platform_user_id (mais explícito e protege ownership na
-- query DELETE).
ALTER TABLE waitlist_items
    ADD COLUMN IF NOT EXISTS cart_id              UUID REFERENCES carts(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS notification_sent_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancelled_at         TIMESTAMPTZ;

-- Suporta status 'cancelled' (cliente desistiu via "Sair da fila"). A
-- tabela hoje não tinha CHECK; adicionamos um para travar valores
-- inválidos. IF EXISTS no DROP é por idempotência caso a migration seja
-- reaplicada num ambiente que já rodou parcialmente.
ALTER TABLE waitlist_items
    DROP CONSTRAINT IF EXISTS waitlist_items_status_check;
ALTER TABLE waitlist_items
    ADD CONSTRAINT waitlist_items_status_check
    CHECK (status IN ('waiting','notified','fulfilled','expired','cancelled'));

-- Suporta a query ListActiveByCart (lista o que mostrar no checkout).
CREATE INDEX IF NOT EXISTS idx_waitlist_cart_active
    ON waitlist_items(cart_id)
    WHERE status IN ('waiting','notified');
