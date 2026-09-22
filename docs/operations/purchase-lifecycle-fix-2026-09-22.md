# Correções de carrinhos vinculados, fila e sincronização

Correções em `fix/erp-linked-purchase-lifecycle`, a partir de `origin/main` (`705d3b8`). Frontend separado em `fix/product-image-fallback`, a partir de `b2748fd`. Nenhuma alteração direta nos pedidos de produção integra esta entrega.

## Comportamento corrigido

- Expiração e cancelamento respeitam a compra principal. Uma tarefa antiga do carrinho vinculado não pode liberar seu estoque nem cancelar a venda paga do principal. O advisory lock usa a identidade do principal; os locks de carrinhos e produtos seguem ordem determinística.
- Erros de banco e contenção de finalização sobem ao Asynq, preservando retentativas. A identidade da tarefa de expiração inclui o vencimento, permitindo agendar uma extensão enquanto a tarefa anterior está ativa.
- Recuperação de expiração a cada minuto, limitada a 50 carrinhos vencidos nas últimas 24 horas. Pagos, reembolsados, VIPs, compras encerradas, revisão financeira e situações fiscais fechadas são preservados. Vencidos mais antigos ficam para conferência operacional.
- Criação, grade e mutação do pedido usam o principal e todos os carrinhos vinculados. Um principal sem itens próprios também consegue criar o pedido com os itens dos vinculados.
- Reflexão do ERP confirma e ajusta a composição inteira em uma transação. Mantém a atribuição aos carrinhos originais e os preços das unidades ainda em espera. Uma falha reverte todos os produtos. Linhas com escrita pendente divergente são preservadas para conciliação.
- Quantidade igual não basta para confirmar um item: é necessária correspondência de quantidade **e preço**. A confirmação local sem grade remota só vale para carrinho independente, sem pedido no ERP. O comprador não recebe confirmação de uma escrita ainda pendente.
- Comentários antigos vinculados a compras encerradas ficam aguardando conciliação, com os itens preservados. A recuperação não insiste em editar a mesma compra fechada.
- Cancelamento Tiny que recebe 404 verifica o mesmo ID com GET, na mesma conta. Somente outra resposta 404 permite concluir como venda ausente. A referência é preservada; o fluxo não cria uma nova venda nem devolve estoque local novamente.
- Leitura de grade faturada é separada da consulta usada antes de mutações. A conciliação pode conferir a nota, enquanto a edição permanece protegida.
- Aviso de comentário de live encerrada fica como não entregue, motivo `live_ended`. Posts/reels mantêm sua janela própria; mensagens diretas seguem seu caminho separado.
- A recuperação de estoque mantém o limite de dez produtos por conta/minuto. Até oito posições priorizam fila, vendas ativas e falhas; o restante mantém rotação do catálogo. Isso não significa atualização de todo o catálogo a cada quinze minutos.
- Leituras de estoque têm uma concessão temporária compartilhada por produto e revisões duráveis. Eventos sobrepostos podem compartilhar uma leitura; uma nova atualização invalida a leitura antiga e impede a promoção da fila até confirmar o saldo mais recente. O bloqueio não mantém conexão PostgreSQL ocupada durante a chamada à Tiny.
- No frontend, fotos recusadas tentam outras URLs fornecidas pelo ERP e depois mostram indicação acessível de imagem indisponível. Uma URL 403 da Tiny continua exigindo correção na origem.

## Publicação

A migration `000169_stock_read_recovery` acrescenta quatro colunas à tabela de checkpoints existente, uma linha por produto. Não exclui dados, não recria tabelas de logs e não faz backfill comercial. Aplicar antes de iniciar o novo backend; o fluxo normal de migrations do deploy atende essa ordem.

Os fixes incluem componentes compartilhados de carrinho/ERP, portanto também foram executados os testes dos demais provedores. A entrega não contém o trabalho de Nuvemshop de outros worktrees. O fluxo de publicação usa commits diretos em `stg` e branches separadas a partir da `main` para os PRs de produção, abertos e aprovados pelo usuário. Os PRs não carregam os demais trabalhos de staging.

## Validação

Banco PostgreSQL local descartável, com migrations reais; ERP e Instagram simulados. Nenhuma API de escrita de cliente real foi usada.

- Os sete cenários que falhavam no diagnóstico passaram como regressões: perda de erro de expiração, confirmação indevida de vinculado, duplicação por reflexão, grade vazia do vinculado, liberação de estoque pago, replay em compra encerrada e cancelamento da venda paga.
- Casos adicionais: retry idempotente, lock compartilhado, cancelamento manual protegido, criação disparada pelo vinculado, divergência de preço, 404 confirmado versus 200/403, leitura fiscal sem liberar escrita, origem/preço em reflexão e rollback integral.
- Estoque: concessão concorrente, recuperação priorizada com rotação, revisão recebida durante GET, rejeição do saldo superado e fila aguardando confirmação.
- `go test -tags integration ./apps/api/...` e `go build ./apps/api/...`.
- Testes de concorrência selecionados com `-race`.
- Frontend: 13 testes de importação Playwright e `npm run build`.

### Revalidação ampliada em 22/09

A pedido do usuário, a validação foi ampliada para PostgreSQL **e Redis locais reais**, detecção de corridas e ordem embaralhada. A primeira rodada embaralhada identificou dependência de ordem no teste `TestVarreduraParaDePerguntarPorPedidoApagadoNoERP`: a venda recém-criada podia ficar fora do lote global de 100 pedidos devido aos dados de testes anteriores. O cenário agora torna a venda explicitamente antiga e usa a janela correspondente; as verificações de 404, manutenção do vínculo e ausência de nova consulta foram preservadas.

Foram adicionadas regressões para:

- Agendar o novo vencimento enquanto a tarefa anterior continua ativa no Redis, tanto com identificador antigo quanto com o novo formato; repetir o agendamento permanece idempotente.
- Aplicar a migration 169 sobre registros existentes, reverter apenas essa migration e reaplicá-la, preservando estoque, preço e os checkpoints anteriores.
- Preservar a revisão pendente e liberar a concessão de leitura após falha do ERP ou cancelamento da requisição.
- Impedir que uma revisão já concluída ignore uma edição comercial posterior; após conciliação, exigir nova leitura.
- Executar o caminho de serviço de estoque enquanto uma reserva ainda está viajando ao ERP e quando a confirmação chega durante o GET. A unidade não volta ao disponível e também não é descontada duas vezes após a confirmação.

Comandos de validação (variáveis apontando exclusivamente para serviços locais):

```sh
go test -race -tags=integration -p=2 -shuffle=1790087464715024024 -count=1 -timeout=5m -json ./apps/api/...
go vet -tags integration ./apps/api/...
go build ./apps/api/...
npm run test:erp-import -- --repeat-each=3
npm run test:waitlist
npm run lint
npm run build
```

Resultado final do backend: **41 pacotes aprovados, 1.336 testes de primeiro nível aprovados, três desativados, nenhuma falha e nenhum relato do detector de corridas**. A contagem de testes não soma novamente seus subcasos. `go vet` e build também passaram. Os resultados JSON completos desta execução ficaram em `/tmp/livecart-revalidate-final.jsonl`.

O frontend passou em 39 execuções de importação (13 cenários, três repetições), seis cenários de fila, lint e build. O lint/build mantém avisos preexistentes em outros arquivos, sem erros nos arquivos alterados.

**Limites da evidência:** três testes preexistentes permanecem explicitamente desativados e não contam como aprovados: `TestOnCartPaid_AC2_RetryOnInsertError` (o próprio teste declara cobertura pelos cenários AC7/AC9), `TestEspelhoDoERPNuncaInventaUnidade` e `TestDefeitoEspelhoAplicaLeituraAnteriorAoNossoMovimento` (diagnósticos antigos do método bruto de estoque, sem passar pelo desconto de promessas do serviço). A nova regressão `TestTinyStockRefreshDoesNotReofferUnacknowledgedUnit` cobre esse intervalo pelo caminho de serviço utilizado pelas notificações de estoque. As suítes com tags `e2e`, `tiny_search_e2e`, `tiny_checkout_e2e` e `external` exigem configuração explícita de contas externas e não foram executadas. Os cenários de navegador aqui usam API simulada; o banco e a fila são reais, locais e descartáveis. Isso não substitui a homologação do deploy e das credenciais/webhooks em staging.

## Conciliação que permanece separada

Publicar o código não decide o destino de intenções comerciais antigas nem reexecuta tarefas arquivadas automaticamente.

- Compra 1480/1518: conferir a composição histórica duplicada e as unidades já presentes na Tiny; não adicionar novamente por presunção.
- Carrinho 1235: definir o destino das quatro unidades associadas à compra anterior, cujo vínculo Tiny não existe mais.
- Carrinhos 1013/1020: após confirmar o vínculo ausente, retomar especificamente os cancelamentos arquivados.
- Esperas antigas dos carrinhos 1040, 1193, 1214, 1394 e 1525: conferir as sete unidades VIP antes de reativar. A espera do 1331 é de carrinho comum vencido.
- Nove unidades da recuperação manual original: conferir se o lojista já adicionou, além de pagamento e disponibilidade atual. Não reinserir automaticamente.
- Os vencidos antigos, inclusive vendas faturadas manualmente, exigem conferência antes de retomar expiração.

## Logs úteis após publicar

- `stale cart expiry ignored for protected purchase`
- `cart expiry recovery deferred`
- `purchase reconciled from ERP order` (`changes`, `deferred_products`)
- `ERP item recovery awaits verified grid`
- `ERP cancellation reconciled: order already absent`
- `notification not delivered: live reply window closed`
- `ERP available stock reconciled` e `Tiny stock recovery batch finished`

Conflitos reais de versão de estoque continuam retentáveis. Suprimir essa proteção permitiria sobrescrever uma alteração mais recente.
