-- O down NÃO remove billing_interval.
--
-- Esta migration é um reparo idempotente: em produção ela não criou nada, e
-- derrubar a coluna aqui apagaria o que a 000139 legitimamente criou lá. O
-- inverso de "garanti que existe" é não fazer nada — quem quiser desfazer a
-- feature desfaz a 000139.
SELECT 1;
