-- Sem rollback, de propósito.
--
-- A up é uma migração de DADOS: reconstrói Orders de vendas reais a partir do
-- cart de origem. Depois de rodar, essas linhas são indistinguíveis das criadas
-- pela materialização normal (é justamente o objetivo) — um DELETE aqui teria
-- que adivinhar quais apagar e apagaria venda de verdade.
--
-- Reverter o schema não requer nada: a 000103 não cria nem altera estrutura.
SELECT 1;
