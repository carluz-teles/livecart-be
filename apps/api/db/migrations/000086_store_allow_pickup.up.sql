-- "Permitir retirada na loja": quando ligado, o checkout sempre oferece a
-- opção "Retirar na loja" (grátis, com o endereço da loja) junto das opções
-- de entrega — e vira o fallback quando a loja não tem integração de frete.
ALTER TABLE stores
    ADD COLUMN IF NOT EXISTS allow_store_pickup BOOLEAN NOT NULL DEFAULT false;
