# Remoção da persistência de logs HTTP

A tabela `integration_logs` acumulava os corpos completos das requisições e
respostas de provedores. Na medição de produção de 13/09/2026, ocupava
280.444.928 bytes (aproximadamente 267 MiB), cerca de 80% do banco.

## Mudança

- A migration `000154_remove_integration_logs` remove somente essa tabela, com
  `DROP TABLE ... RESTRICT`, sem `CASCADE` e com espera por lock limitada a 5s.
- As queries e o método de persistência foram removidos; SQLC foi regenerado.
- O callback dos provedores continua reativando integrações em `error` após uma
  chamada bem-sucedida. Webhooks, filas, pagamentos e regras de retry permanecem.
- O logger da aplicação recebe metadados: integração, método, status HTTP,
  duração e contexto da loja. Falhas aparecem como `integration operation failed`
  em `warn`; sucessos ficam em `debug`. Esse callback não registra payloads nem
  mensagens cruas de erro. Os avisos existentes de HTTP 429 continuam disponíveis.

## Publicação

Branch criada a partir de `origin/main` em `3bdd467`, sem commits de staging ou
da integração Nuvemshop.

Produção usa `RUN_MIGRATIONS_ON_STARTUP=true`, confirmado pela Railway CLI.
A migration é executada no boot do próximo deploy. O banco precisa aceitar
conexões e ter espaço para concluir a transação e registrar a versão da migration.
Ela não contorna `the database system is in recovery mode` ou disco cheio.

Durante a troca de containers, o binário antigo pode emitir `failed to log
integration operation` porque a tabela já foi removida. O callback antigo trata
essa falha como aviso, sem invalidar a resposta do provedor.

Após o deploy, conferir `migrations applied` e executar em modo somente leitura:

```sql
SELECT version, dirty FROM schema_migrations;
SELECT to_regclass('public.integration_logs') AS tabela_removida,
       pg_size_pretty(pg_database_size(current_database())) AS tamanho_banco;
```

O resultado esperado é versão 154 sem `dirty` e `tabela_removida = NULL`.

## Rollback e branches ainda não publicadas

O down recria a estrutura anterior vazia, com a mesma FK. Não recupera os logs
apagados. Para voltar ao binário anterior sem avisos de tabela inexistente,
restaurar primeiro essa estrutura.

A branch local da Nuvemshop já possui migrations não publicadas a partir de 154.
Antes de incorporá-la, resolver essa colisão de versões e remover a consulta de
`integration_logs` no relatório de privacidade da Nuvemshop. Não aplicar migrations
de outra branch apontando para uma versão já ocupada por uma mudança diferente.

## Validação

Teste com banco descartável cobre upgrade com logs existentes, preservação da
integração e dos webhooks, schema limpo, rollback vazio e reaplicação. O teste do
callback passa uma resposta HTTP real de servidor local e verifica que dados do
cliente e credenciais não chegam ao logger e que a resposta do provedor é mantida.
Os testes existentes de recuperação de status usam o schema sem a tabela.
