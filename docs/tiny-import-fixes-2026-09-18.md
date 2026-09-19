# Busca e importação Tiny — correções e revalidação de 18/09/2026

Correções dos 14 casos da [auditoria](tiny-import-audit-2026-09-17.md), validadas
localmente. A investigação e os testes não alteraram dados de produção.

## Publicação

- Backend: branch `fix/tiny-import-and-approval-production`, baseada na `main`.
- Frontend: branch `fix/tiny-import-search-production`, baseada na `main`.
- As mesmas correções são aplicadas à `stg`; os PRs de produção partem dessas
  branches específicas, preservando os demais trabalhos exclusivos de staging.
- Publicar os dois repositórios na mesma janela, backend antes do frontend.
  O frontend novo usa o endpoint de detalhes e divide a importação de variantes
  em lotes de até cinco; o backend novo recusa lotes maiores.
- O pacote backend também inclui a correção de
  [aprovação de pedidos na Tiny](tiny-approval-and-import-search.md) e a
  conciliação de edições pendentes, com migration **160**, que adiciona dois
  campos de controle. Não exclui dados nem corrige pedidos históricos por
  inferência. A busca/importação em si não exige migration adicional.

## O que foi corrigido

| Caso | Comportamento após a correção | Verificação |
| --- | --- | --- |
| B01 | O cadastro simples consulta novamente a integração Tiny da própria loja antes de salvar. Usa o disponível atual e os identificadores retornados pelo ERP; preserva os campos revisados no formulário. | Prévia com 5 unidades, saldo mudando para 0 antes de gravar: salva 0, SKU e GTIN preservados. |
| B02 | Estoque desconhecido bloqueia a importação com resposta temporária; não é persistido como zero. Zero confirmado continua sendo um saldo válido para cadastro. | PostgreSQL local, produto simples e variante; falha na variante não deixa grupo parcialmente gravado. |
| B03 | Enriquecimento transporta a validade do saldo; falha na releitura invalida `StockKnown`. | Releitura sem saldo disponível não transforma zero desconhecido em zero confirmado. |
| B04 | Produto ou variante inativa é recusada na confirmação. | Produto ativo na prévia e inativo na releitura não é criado. |
| B05 | Importação e cadastro simples Tiny usam 50 s no navegador e operação de até 45 s no servidor. | Resposta após 10,2 s conclui; cancelamento e prazo abrangem o corpo HTTP. |
| B06 | Abrir um pai lê somente o catálogo; o disponível das variantes é confirmado após a escolha. A interface processa até 5 variantes por etapa, com progresso. | Pai com 22 variantes abre com uma chamada, usando o limitador real e tempo virtual. Teste de navegador importa sete em duas etapas. |
| B07 | Importação parcial acrescenta somente variantes ausentes ao mesmo grupo. Retentativas e chamadas concorrentes preservam os registros existentes. A interface identifica grupo iniciado e variantes já cadastradas. | Azul, vermelha, repetição, quatro chamadas simultâneas; uma linha por identidade externa. Falha na segunda etapa e retomada pela interface. |
| B08 | Erro temporário conhecido retorna `503/ERP_THROTTLED`, mensagem pública fixa e `Retry-After`. Outros erros internos continuam mascarados. | Contrato HTTP, incluindo tentativa de expor mensagem privada de provedor. |
| B09 | Produto excluído na Tiny retorna 404. | GET Tiny 404 atravessa serviço e serialização HTTP sem virar 500. |
| B10 | Conflito de cadastro ou código retorna 409. | Tentativa duplicada deixa apenas um registro. |
| B11 | Identificação rápida de GTIN considera comprimentos 8, 12, 13 e 14; código numérico longo continua sendo pesquisado como SKU. | SKU numérico de 25 dígitos, código de barras e fallback para SKU de 8 dígitos. |
| B12 | Timer e cancelamento permanecem ativos até terminar a leitura do JSON. | Corpo pendente após headers, timeout e cancelamento externo. |
| B13 | Seletor suporta ausência de `attributes`. | Componente real renderizado no Chromium sem erro. |
| B14 | Resultados da busca anterior deixam de ser selecionáveis imediatamente durante o debounce. | Troca de termo e tentativa de seleção antes dos 500 ms. |

## Organização e compatibilidade

- O `GetProduct` normal da Tiny continua lendo o disponível. A leitura leve é uma capacidade específica do caminho de importação, sem retirar a confirmação de estoque do SYNC.
- Cada lote de variantes é transacional. Se um lote falhar, nenhum item daquele lote é gravado; os anteriores permanecem. A retomada não sobrescreve estoque, imagens ou preços de variantes existentes.
- Um bloqueio transacional por loja serializa a criação/continuação de grupos ERP concorrentes. A chave única continua sendo a proteção final contra duplicação.
- A API recusa lote Tiny acima de cinco variantes antes de gravar. O frontend envia os lotes automaticamente. Publicar backend e frontend juntos evita incompatibilidade com a importação antiga de grupos grandes.
- O cliente HTTP compartilhado teve o ciclo de cancelamento corrigido, e o tratamento HTTP compartilhado ganhou a exceção pública restrita a `ERP_THROTTLED`. As suítes dos outros fluxos também foram executadas.
- A suíte de convenções foi ajustada para os métodos movidos, validação nova e guards de configuração já existentes na base. O teste de rajada em pedido pago agora executa a retentativa prevista para `ErrOrderBusy`, em vez de supor conclusão após um `Sleep` de 50 ms; a regra de negócio desse fluxo não mudou.

## Medição na Tiny de testes

Sessão OAuth renovada somente no banco local. Identidade ADABYTE LTDA conferida antes das leituras. Não foram alterados produtos, estoque ou pedidos externos.

| Operação | Chamadas GET | Tempo observado |
| --- | --- | --- |
| Busca por nome/SKU de produtos marcados para teste | 2 | 4,931 s |
| Detalhes do produto escolhido + estoque disponível | 2 | 5,040 s |
| Busca pelo código de barras do produto escolhido | 1 | 2,476 s |

O produto marcado retornou **10 unidades disponíveis**. Os tempos incluem espera no limitador conservador de 2,5 s por chamada; não são um SLA nem uma medição da conta Canto da Art.

## Resultado da validação

- Suíte completa `go test -p 2 -tags=integration ./...`: **40 pacotes com testes passaram** (demais pacotes sem testes), incluindo migrations, Tiny, Bling, pagamentos e catálogo.
- Frontend: **51 testes passaram** — 36 de operações/catálogo/busca, 7 de comentários e 8 de importação no navegador.
- Detector de corridas: casos de busca/importação com banco local, importação concorrente, repetição idempotente e continuação de grupos.
- Cenário de rajada em pedido pago: **30 repetições com `-race` passaram**, conferindo a grade final após a retentativa prevista.
- `go build ./apps/api/...`, `npm run build` e verificação de textos ERP: aprovados. O build frontend mantém avisos preexistentes de lint/estilos/Browserslist.
- Teste real de busca/estoque na Tiny de testes: aprovado, conforme a medição acima.

## Executar a regressão

Backend, somente com URL de PostgreSQL local/descartável:

```sh
TEST_DATABASE_URL='<postgres-local>' go test -p 2 -tags=integration ./... -count=1
go build ./apps/api/...
TEST_DATABASE_URL='<postgres-local>' go test -race -tags=integration ./apps/api/internal/integration -run '^Test(Audit|TinyImport|TinyBarcode|Search)' -count=1
```

Frontend:

```sh
npm run test:operations
npm run test:comments
npm run test:erp-import
npm run build
```

`test:erp-import` inicia um aplicativo Next local na porta 3211, monta os componentes reais e controla as respostas HTTP com Playwright. Não usa autenticação ou credenciais reais. O fixture está versionável em `test-fixtures/erp-import-ui`.

Os testes de regressão de importação do backend participam agora das suítes normais; os que escrevem em PostgreSQL continuam separados pela tag `integration`.

## Observabilidade e limites

Logs de busca e seleção registram duração, número de resultados/chamadas e sucesso por integração. `ERP product import completed` informa loja, integração, produto externo, número selecionado, número importado, duração e resultado para cada etapa, inclusive falhas.

Timeouts continuam protegendo operações travadas. Uma Tiny indisponível ou com cota esgotada pode recusar temporariamente a leitura; isso passa a ter mensagem adequada e retomada, sem inventar estoque nem deixar a interface presa. A importação confirma um retrato do saldo disponível, não reserva estoque contra vendas simultâneas realizadas fora do LiveCart.
