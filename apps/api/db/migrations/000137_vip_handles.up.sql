-- vip_handles guarda os @ que a loja marcou como CLIENTE VIP. Espelha
-- blocked_handles (o oposto conceitual): uma linha por (loja, @), "ativa"
-- enquanto removed_at IS NULL. Remover não deleta — mantém auditoria e reativa
-- por upsert, igual ao bloqueio.
--
-- O QUE UM VIP GANHA: o carrinho dele nunca expira (carts.never_expires) e
-- acumula itens de qualquer evento no MESMO carrinho, até ser pago ou cancelado.
-- Ver 000138 (never_expires + store_id + resolução cross-evento).
CREATE TABLE IF NOT EXISTS vip_handles (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id          UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    platform_handle   VARCHAR NOT NULL,
    added_by_user_id  UUID REFERENCES users(id),
    added_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    removed_at        TIMESTAMPTZ,
    removed_by_user_id UUID REFERENCES users(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (store_id, platform_handle)
);

CREATE INDEX IF NOT EXISTS idx_vip_handles_active
    ON vip_handles (store_id, platform_handle)
    WHERE removed_at IS NULL;
