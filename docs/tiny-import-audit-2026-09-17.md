# Auditoria de busca e importação Tiny — 17/09/2026

> Atualização em 18/09/2026: os 14 casos foram implementados após autorização. Veja [correções e revalidação](tiny-import-fixes-2026-09-18.md). O texto abaixo preserva as evidências anteriores às correções.

## Escopo e limites

A pedido do usuário, **nenhuma correção nova foi aplicada nesta auditoria**.
O objeto auditado é o código local com as melhorias de busca já feitas no turno
anterior. As falhas abaixo precisam ser decididas antes da publicação. Não são
14 ocorrências comprovadas na Canto da Art: são 14 problemas reproduzidos no
código, no navegador e/ou em banco descartável, conforme a evidência indicada.

Produção não recebeu alterações. A conta externa de testes foi identificada como
ADABYTE LTDA e recebeu apenas GETs neste ciclo. Estoque, cadastros e concorrência
foram simulados localmente; gravações ocorreram em banco PostgreSQL descartável.
O formulário real de busca foi montado em um aplicativo de testes separado, com
autenticação fictícia e respostas HTTP controladas; o código do componente não
foi copiado nem alterado para os testes.

Não há comprovação de todos os estados possíveis de um serviço externo. A matriz
abaixo explicita os cenários verificados e as lacunas restantes.

## Lista completa de problemas encontrados

| ID | Prioridade | Problema e evidência | Efeito |
| --- | --- | --- | --- |
| B01 | Alta | Produto simples é salvo com o saldo da prévia. Teste: disponível muda de 5 para 0 após seleção; o cadastro grava 5. | Pode começar com estoque acima do ERP. |
| B02 | Alta | Importação final aceita `StockKnown=false`. Teste com PostgreSQL gravou estoque 0 e devolveu sucesso. | Falha de consulta vira informação falsa de produto esgotado. |
| B03 | Alta | Releitura de variante copia `Stock=0`, mas preserva `StockKnown=true` da leitura anterior. | Erro de estoque pode se transformar em zero aparentemente confirmado. |
| B04 | Alta | Importação final aceita produto inativo. Teste retornou `Active=false` do ERP e o cadastro local foi criado ativo. | Produto retirado de venda no ERP pode ser reofertado localmente. |
| B05 | Alta | O POST de confirmação da importação continua com timeout de 10 s. Resposta simulada em 10,2 s foi abortada com 408. | A tela informa falha e o backend pode continuar importando; repetir pode resultar em conflito. |
| B06 | Média | Selecionar um pai exige ler o estoque de todas as variantes antes de escolher qualquer uma. Com 22 variantes e intervalo conservador de 2,5 s, só 18 chamadas couberam no prazo: falhou após 42,517 s. | Grupos grandes continuam inacessíveis mesmo com saldo positivo e ERP respondendo normalmente. |
| B07 | Média | Importar azul e depois vermelha do mesmo grupo retorna “grupo de produto já importado”. A listagem ainda mostra `alreadyImported=false` para o pai. | A importação parcial impede completar o catálogo depois e a tela continua oferecendo a ação. |
| B08 | Média | `ERP_THROTTLED`/503 e mensagem de estoque desconhecido são substituídos por `reason=INTERNAL`, `error=internal server error` pelo tratamento HTTP. | O aviso útil não chega à tela, embora o teste do serviço isolado passe. |
| B09 | Média | Produto removido entre busca e seleção: Tiny 404 é transformado em erro genérico e chega como HTTP 500. | Remoção legítima aparece como defeito interno e não explica como refazer a busca. |
| B10 | Média | Salvar de novo o mesmo produto pelo formulário simples viola a chave única e devolve 500. O teste confirmou somente uma linha persistida. | Não duplica o cadastro, mas apresenta erro interno em vez de produto já cadastrado. |
| B11 | Baixa | Todo número de oito ou mais dígitos é considerado GTIN. Entrada numérica de 25 dígitos causa 400 real na Tiny; o caminho novo encerra antes de tentar SKU. Teste roteirizado confirmou zero consultas por SKU mesmo com produto correspondente. | SKU numérico longo deixa de ser pesquisável pela heurística de código de barras. |
| B12 | Média | Timer e cancelamento do cliente HTTP são removidos ao receber os headers, antes de ler o JSON. Teste com orçamento de 20 ms permaneceu pendente aos 80 ms; cancelar externamente depois dos headers não abortou a leitura. | Resposta incompleta pode ficar presa e consultas obsoletas continuar lendo dados. |
| B13 | Média | `attributes` é opcional no JSON de variante, mas o seletor executa `Object.entries` sem proteção. Chromium reproduziu “Cannot convert undefined or null to object”. | A janela de variantes quebra com uma resposta permitida pelo contrato. |
| B14 | Média | Durante os 500 ms de debounce, os resultados anteriores continuam selecionáveis. No navegador, trocar “Vela” por “Outro produto” e confirmar rapidamente emitiu `onSelect(p1)` do termo anterior. | O usuário pode avançar com um produto da busca anterior. |

### B01 — prévia não é confirmação do estoque

Caminho: `ProductForm/index.tsx` → `useCreateProduct` → `POST /products` →
`product.Service.Create`. O formulário envia o estoque recebido anteriormente;
o serviço de cadastro não relê o ERP nem valida a versão dessa leitura. O campo
ser somente leitura na interface não impede que o saldo envelheça. A reprodução
usa as mesmas operações de detalhe, DTO e serviço de cadastro do fluxo.

Teste: `TestAuditSimpleFormRevalidatesBeforeSaving`. O mesmo teste confirmou
preservação de SKU `000123` e GTIN; a falha observada foi o saldo.

Direção de correção: confirmação de importação pelo backend, validando a fonte e
os dados atuais antes de gravar, com tratamento de concorrência e duplicidade.

### B02 / B03 / B04 — validação final e segunda leitura

Caminhos: `integration.Service.ImportERPProduct`,
`enrichVariantsFromIndividualGets` e adaptadores de produto/grupo.
A nova proteção do endpoint de detalhes não protege a releitura feita pelo POST.
O adaptador de cadastro usa o valor numérico do estoque e a criação assume ativo.
No enriquecimento, a leitura seguinte substitui o saldo sem transportar sua
validade. Um número zero desconhecido não deve ser tratado como zero confirmado.

Testes: `TestAuditImportRechecksActiveAndKnownStock` e
`TestAuditVariantReReadCannotTurnUnknownStockIntoZero`.

Direção: validar novamente na gravação e propagar explicitamente a validade do
saldo; decidir a operação quando alguma variante não puder ser confirmada.

### B05 / B06 — o trabalho longo foi deslocado, mas ainda existe

Busca e detalhes receberam novos prazos no turno anterior. Entretanto,
`integrationService.importProduct` usa `apiClient.post` com o padrão de 10 s.
O handler de importação usa `c.Context()` sem um prazo próprio da operação.
Importar um pai pode fazer `2 + N` leituras iniciais e depois mais duas por
variante selecionada para enriquecer dados, além de eventuais leituras de frete.
Aumentar só o timeout do GET não resolve essa confirmação.

Testes: “confirmar importação aguarda mais de dez segundos” e
`TestAuditLargeVariantSelectionWithinBudget`. O teste de 22 variantes usa tempo
virtual e o limitador real, não gera rajadas nem 429 na conta Tiny. O resultado
depende da cota: não afirma que qualquer conta com 22 variantes sempre falha.

Direção: carregar/confirmar apenas as variantes escolhidas, evitar releituras
redundantes, definir um resultado recuperável para trabalho que ultrapasse a
requisição e alinhar o prazo da confirmação com esse comportamento.

### B07 — importação parcial sem continuação

`productgroup.SyncerAdapter.ImportFromERP` rejeita qualquer grupo já existente.
A escolha de subset funciona apenas na primeira importação. O marcador da busca
consulta produtos individuais e não reconhece o ID externo do pai no grupo.

Teste: `TestAuditPartialGroupCanImportRemainingVariant` grava azul, consulta a
listagem e tenta vermelha. O segundo cadastro é recusado, sem alteração do primeiro.

Direção: adicionar somente variantes ausentes ao grupo existente, com identidade
externa e transação; mostrar corretamente quais já estão cadastradas.

### B08 / B09 / B10 — contrato HTTP

- `httpx.HandleServiceError` torna **todo** `ServiceError` 5xx genérico. Não se
  deve remover a proteção global contra exposição de erros internos; será preciso
  um contrato seguro para indisponibilidade esperada do ERP.
- `Tiny.GetProduct` devolve erro textual para 404, perdendo a classificação.
- `product.Repository.Save` propaga a violação de unicidade como erro genérico.

Testes: `TestAudit503MessageSurvivesHTTP`,
`TestAuditDeletedSelectionRetainsNotFound`, `TestAuditDuplicateSimpleFormIsConflict`.
O último caso preserva uma única linha: o problema é contrato/recuperação, não
uma duplicação comprovada.

### B11 — regressão da prioridade do GTIN

Este é um problema da proposta local ainda não publicada. A heurística `isGTIN`
aceita qualquer sequência com pelo menos oito números, sem limite superior.
Antes havia tentativa paralela por SKU; o novo caminho encerra na falha do GTIN.
Oito dígitos normais, como `47169001`, receberam vazio com sucesso e permitem
fallback. Já a entrada longa recebeu 400 na API de testes.

Teste: `TestAuditLongNumericSKUStillSearchable` + `audit-live-search.json`.
Não foi identificado um SKU de cliente em produção afetado por esse caso.

Direção: reconhecer formatos plausíveis e preservar a busca por SKU quando a
entrada não puder ser interpretada como GTIN; não confundir isso com retry de 429.

### B12 — ciclo de vida incompleto da requisição

Em `src/services/api/client.ts`, `send()` devolve o `Response` e executa o `finally`
antes de `res.json()`. A leitura do corpo fica sem o timer e sem o vínculo com o
sinal externo. A falha pode afetar outros GETs que usam o mesmo cliente.

Dois testes isolados comprovam timeout e cancelamento após headers. O teste
fecha o stream ao terminar; não deixa requisições presas no processo.

### B13 / B14 — interface real

A aplicação de teste importa o `ProductFormERPSearch` e o seletor reais, com
React Query real. Apenas autenticação, loja e respostas HTTP são controladas.
O teste normal de localizar e confirmar um produto simples passou.

- Variante sem atributos: resposta compatível com `attributes,omitempty` do
  backend, abertura do modal e erro JavaScript observado no Chromium.
- Troca de termo: relógio controlado mantém o teste dentro dos 500 ms de debounce;
  uma resposta antiga ainda pode alimentar `onSelect`, reproduzido como `p1`.

Direção: aceitar campos opcionais e invalidar a seleção/lista assim que o texto
visível mudar, sem esperar o disparo da próxima requisição.

## Cobertura e evidências

| Área | Exercício | Resultado |
| --- | --- | --- |
| Tiny real | GTIN existente, inexistente, entrada de oito dígitos, número longo, nome e SKU | Seis GETs; cinco respostas 200 e um 400 esperado para a entrada longa |
| Disponível | Físico 10, reservado 7, disponível 3 | Importação seleciona 3, sem usar o físico |
| Falta de saldo | Disponível confirmado em zero | Detalhe recusa com 422 |
| Saldo desconhecido | Campo ausente ou JSON inválido | Detalhe recusa com 503; mensagem HTTP ainda tem B08 |
| Identificadores | Acentos, espaços, `&`, barra e zeros iniciais no parâmetro; SKU/GTIN no cadastro | Preservados nos testes |
| Segurança de loja | Integração de outra loja | Rejeitada antes de consultar o provider |
| Duplicidade | Segundo cadastro com mesma origem/ID externo | Banco impede duplicação; resposta tem B10 |
| Produto simples | Browser, prévia, seleção e confirmação | Controle passou |
| Timeout | Listar/detalhar em 10,2 s | Testes anteriores continuam passando |
| Timeout de importação | POST em 10,2 s | B05 reproduzido |
| Corpo parcial/cancelamento | Headers recebidos, JSON ainda pendente | B12 reproduzido |
| Variantes | Estoque parcial, grupo grande, seleção parcial e grade sem atributos | B03, B06, B07 e B13 reproduzidos |
| Regressão | Integração, provider Tiny/Bling, produto, grupos e rate limiter com PostgreSQL local | Suítes existentes passaram |
| Regressão frontend | Operações, catálogo, billing, progresso e busca | 36 testes passaram |

Foram executados 17 testes principais de auditoria: 11 no backend e seis no
frontend, além dos subcasos. Nove testes do backend e cinco do frontend
reproduziram falhas; três controles passaram. Alguns testes cobrem mais de um
sintoma da mesma causa, conforme a lista de 14 problemas.

Os testes de auditoria são separados e opt-in. Estão **vermelhos de propósito**
quando exigem o comportamento correto e reproduzem um defeito ainda não corrigido:

- Backend: `product_import_audit_test.go` e `product_import_audit_db_test.go`,
  sob as tags `tiny_import_audit` e `integration`.
- Frontend: `tests/erp-import.audit.ts`, executado exclusivamente com
  `playwright.import-audit.config.ts`.
- Logs e resultados privados: `.tiny-demo/incident-tiny-payment-search-20260917/`,
  arquivos `audit-backend-final.log`, `audit-frontend-final.log`,
  `audit-regression.log`, `audit-fe-regression.log` e `audit-live-search.json`.
- Servidor de UI descartável: `.tiny-demo/import-audit-ui`; não faz parte da
  aplicação publicada nem usa credenciais de clientes.
- Hash dos arquivos da aplicação capturado antes/depois: nenhuma nova alteração
  de implementação neste ciclo. Apenas testes, configuração de teste e relatório.

## O que ainda não foi comprovado

Não foram provocados limites reais de cota, indisponibilidade, expiração de token
ou falhas de rede na conta do cliente. Esses casos foram simulados ou cobertos
pelos testes existentes. A busca com código de barras foi exercitada em dados de
teste; não se afirma que cada produto do catálogo da Canto da Art foi consultado.
Kits/fabricados, múltiplos depósitos e todos os formatos de grade possíveis da
Tiny não foram validados um a um no painel externo nesta auditoria.

Os 14 itens são a lista completa dos problemas **confirmados neste escopo**,
sem promessa de ausência de outros defeitos. Para aprovar correções, a ordem
recomendada é B01–B05 (consistência e confirmação), B06–B07 (variantes),
B08–B12 (contratos/tempo) e B13–B14 (interface). Nenhum deles foi corrigido agora.
