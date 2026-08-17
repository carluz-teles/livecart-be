-- Busca de arrobas para a lista de perfis bloqueados.
--
-- A plateia da live vive em live_comments; a tabela `customers` só ganha linha
-- depois que alguém compra. Quem o lojista mais precisa bloquear — a própria
-- conta secundária, que instrui a audiência mandando código de produto — pode
-- nunca ter virado cliente, então a busca tem de ler o log de mensagens.
--
-- Índice funcional em lower(platform_handle): a busca exata compara por
-- minúsculas (o arroba digitado no painel pode vir com maiúscula ou @) e o
-- agrupamento por handle usa a mesma expressão. A busca por trecho continua
-- varrendo os comentários da loja, o que é aceitável no volume de uma live.
CREATE INDEX IF NOT EXISTS idx_live_comments_handle_lower
    ON live_comments (lower(platform_handle));
