# Importação Tiny: busca por código de barras e timeouts

Investigação de 17/09/2026. Produção foi consultada somente para leitura de logs.
Validação externa feita na conta Tiny de testes ADABYTE, sem alterar produtos ou
pedidos neste teste de busca. As alterações ainda dependem de publicação.

## Por que aparecia “A busca demorou demais”

O cliente HTTP do frontend aborta por padrão em 10 segundos e transforma o
cancelamento em erro 408. Essa mensagem era produzida no navegador: não prova
que a Tiny devolveu HTTP 408 nem que o produto inexiste.

A busca antiga executa nome e SKU e, para uma entrada numérica com oito ou mais
dígitos, GTIN. Ela espera todas as consultas. Depois busca detalhes e disponível
de até 20 resultados antes de entregar a listagem. Para produtos simples são
até 43 chamadas: três listagens + 20 detalhes + 20 estoques. Variações acrescentam
uma leitura de disponível por variante.

Mesmo com **um único produto**, o caminho antigo podia exigir cinco chamadas
(três filtros + detalhe + estoque). No intervalo conservador inicial do código,
de 2,5 s entre envios, isso já ocupa cerca de 10 s antes da última resposta, sem
contar concorrência. O intervalo é adaptado quando há cabeçalhos de cota; essa
conta não pressupõe que o plano da Canto da Art tenha o intervalo inicial.

As goroutines não removem a cota da Tiny. O limitador compartilha o orçamento de
leitura da conta entre busca, sincronização e demais operações, inclusive entre
réplicas. Na amostra de logs da Canto da Art houve espera de **8,791 segundos**
antes do envio de uma chamada. Esse log isolado não identifica uma busca
específica; comprova a pressão no orçamento compartilhado. A latência HTTP vem
depois dessa espera. Não foi necessário disparar buscas na conta de produção.

O primeiro teste local que mediu 243 ms não ativava o limitador. Não representava
essa parte do fluxo e foi substituído por validação com o limitador Tiny ativo.

## Correções

- GTIN na Tiny é consultado primeiro. Se houver resultados, não dispara buscas
  adicionais de nome e SKU. Se a resposta for vazia com sucesso, mantém o fallback
  para códigos SKU numéricos, como `47169001`. Falha no GTIN não vira ausência.
- Listagens de importação usam `summary=true` e retornam prévias. Detalhes,
  imagens, dimensões e estoque são consultados ao escolher o produto.
- Uma listagem vazia bem-sucedida não esconde a falha de outro filtro: se nenhum
  produto foi encontrado e uma consulta falhou, a resposta preserva o erro.
- Estoque desconhecido é indisponibilidade para consulta (503), e saldo conhecido
  igual a zero continua sendo falta de estoque (422). Isso inclui variantes;
  a importação não é liberada com uma grade de estoques parcialmente confirmada.
- A busca pode ser cancelada no navegador ao trocar de termo. A requisição no
  backend permanece limitada pelo seu contexto; não depende da desconexão do
  navegador para encerrar. Debounce e cache curto das prévias evitam repetição.
- A tela oferece nova tentativa e informa quando está consultando o ERP ou os
  detalhes do produto. Não fica repetindo 503 automaticamente.

## Orçamento de tempo

| Operação | Backend | Frontend |
| --- | --- | --- |
| Busca de produtos | 25 s | 30 s |
| Detalhes e disponível do selecionado | 45 s | 50 s |
| Demais GETs do frontend | Conforme endpoint | Mantido em 10 s |

O servidor tem limite de escrita de 60 s; cada chamada HTTP do provider tem
30 s e também respeita o prazo menor do contexto da operação. O backend pode
recusar antes do prazo se a próxima vaga na cota não couber nele.

Os limites são proteções contra espera indefinida. A redução de consultas evita
desperdício e os novos prazos incluem esperas legítimas; não garantem sucesso
durante indisponibilidade da Tiny, cooldown prolongado ou sincronização muito
concorrida. Produtos com muitas variantes ainda exigem mais leituras que um
produto simples e podem precisar de nova tentativa. Não há fila persistente de
pesquisas nem prioridade nova sobre pagamentos nesta alteração.

## Estoque disponível preservado

`GET /produtos/{id}` fornece os dados cadastrais. O saldo físico presente nessa
resposta não substitui `GET /estoque/{id}`, usado para obter o disponível. A prévia
não afirma ter consultado estoque; o frontend aguarda o detalhe antes de liberar
a seleção para cadastro. Não foi introduzido cache de saldo para contornar demora.

Referências: [listagem e filtro GTIN](https://api-docs.erp.olist.com/api-reference/produtos/listar-produtos)
e [dados do produto](https://api-docs.erp.olist.com/api-reference/produtos/obter-produto).
A interpretação do disponível também está coberta pelos testes existentes de
saldo e variantes do provider Tiny e por respostas reais da API.

## Evidências e acompanhamento

- Dois testes reproduziram antes da correção: três chamadas mesmo com GTIN
  encontrado; e 404 incorreto quando um filtro falhava e outro retornava vazio.
- Testes cobrem fallback para SKU numérico, 429, cancelamento, saldo zero versus
  desconhecido e estoque parcialmente conhecido nas variantes.
- O frontend recebeu com sucesso resposta simulada após 10,2 s; o limite antigo
  abortava nessa situação. Consultas canceladas pelo usuário não viram timeout.
- Conta demo, com limitador ativo: busca por nome **5,027 s / dois GETs**;
  detalhes e disponível **5,028 s / dois GETs**; GTIN **2,471 s / um GET**.
  Não são garantias de latência em produção. O teste usa produtos fictícios
  previamente cadastrados e verifica a identidade da conta antes das leituras.
- Logs novos: `ERP product search lookup completed` (filtro, duração, quantidade),
  `ERP product search completed` / `failed` (operação completa) e
  `ERP selected product lookup completed` (detalhe). Os IDs permitem correlacionar
  com `provider request quota wait` e `provider HTTP 429`. Não registram o termo
  digitado nem credenciais.

O modo sem `summary` permanece disponível para clientes antigos. Para eliminar
o enriquecimento de toda a lista na interface é necessário publicar backend e
frontend. Nenhuma mudança de schema ou reparação em produção faz parte desta
correção de busca.
