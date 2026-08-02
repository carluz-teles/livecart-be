-- Reversivel em schema. Os assets retidos no storage NAO sao apagados por esta
-- down (a migration nao conhece o storage) — limpar o bucket e operacao, nao
-- migration. Derrubar a tabela sem limpar deixa objeto orfao no bucket, e esse
-- e o preco consciente de a down existir.
DROP TABLE IF EXISTS session_publish_jobs;
