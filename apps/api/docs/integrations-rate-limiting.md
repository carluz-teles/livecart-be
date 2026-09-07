# Rate limiting das integrações

O controle acontece antes de cada requisição HTTP em `providers.BaseProvider`, incluindo tentativas internas de uma operação. Limitar somente as operações da fila de ERP não limita o número de chamadas que cada operação executa.

## Tiny / Olist v3

`ratelimit.Manager.GetOrCreateTiny` fornece o limitador Tiny. GET/HEAD usam orçamento de leitura; os outros métodos usam escrita. Em produção, `main.go` configura o pool compartilhado e a tabela `api_rate_budgets` coordena as réplicas. É necessário aplicar a migração 151 antes desse código.

- Sem cabeçalhos, cada categoria começa em 24/min, abaixo do menor plano documentado (30/min).
- Uma cota de minuto anunciada ajusta a taxa para 80% do limite. O saldo e tempo restante podem reduzir ainda mais o ritmo.
- Cabeçalhos de rajada, como `Limit: 4 / Reset: 1`, não substituem a cota aprendida de minuto.
- Um 429 suspende somente sua categoria pelo reset anunciado; sem prazo válido, o padrão é um minuto.
- `Retry-After` aceita segundos e data HTTP. Sucesso atrasado não cancela uma suspensão já registrada.
- Réplicas disputam apenas a vaga atual, sem reservar uma fila futura. Se a espera não cabe no contexto, a requisição é recusada antes do envio com `ErrNaoDespachado`.
- Sem pool (testes ou ferramentas locais), há controle em memória com a mesma separação de categorias e espera após 429; ele não coordena processos diferentes.

A identidade é CNPJ normalizado dos metadados quando disponível, ou loja. Na ausência do CNPJ, duas lojas da mesma conta Tiny não são automaticamente agrupadas. Aplicativos externos sempre ficam fora do controle LiveCart e compartilham a cota da conta; a margem de 20% não garante eliminar todos os 429.

A [documentação oficial](https://api-docs.erp.olist.com/documentacao/comecando/limites-de-consulta) define o reset em segundos restantes e o compartilhamento por conta. A [tabela por plano](https://ajuda.olist.com/hubs-e-plataformas-via-api/aplicativos-api-v3-configuracoes-e-utilizacao) é a referência para cotas de leitura e escrita. Não aplicar limites da API v2 à v3.

## Outros provedores

O Bling usa o limitador fixo por conta configurado na fábrica. Outros provedores ainda podem usar `AdaptiveLimiter`, que depende de cabeçalhos e não impõe taxa preventiva enquanto não tem dados. Não presumir que seu estado em memória seja compartilhado entre réplicas ou que diferentes APIs tenham os mesmos contratos.

## Observação e testes

`provider HTTP 429` representa uma resposta HTTP recusada e registra método, caminho e cabeçalhos de quota, sem autorização/query string. Logs dos loops de retry descrevem tentativas e não devem ser somados com ele como novas requisições.

Regressões estão em `lib/ratelimit/tiny_test.go`, `internal/integration/rate_budget_test.go` e nos testes de retry/base/estoque dos provedores. O [guia de staging](staging-live-recovery.md) descreve as alterações, os logs e as lacunas de recuperação ainda existentes.
