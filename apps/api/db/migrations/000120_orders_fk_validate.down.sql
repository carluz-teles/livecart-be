-- No-op DE PROPOSITO. Postgres nao tem "DEVALIDATE": a unica forma de voltar
-- uma constraint validada ao estado NOT VALID e dropar e recriar — que e
-- exatamente rodar a down da 000119 e a up de novo.
--
-- Marcar isto como no-op e mais honesto do que dropar a constraint aqui: quem
-- desce so a 000120 quer desfazer a VARREDURA, nao a regra.
SELECT 1;
