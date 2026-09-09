# Edição de itens no Tiny com fila persistente

A edição de itens pelo painel deixa de esperar as chamadas ao Tiny para responder. A transação local grava a alteração, a reserva de estoque e uma revisão pendente; um worker envia a grade atual do pedido. O painel distingue alteração salva de sincronização concluída.

## Contrato e abrangência

`POST /orders/{id}/items`, `PATCH /orders/{id}/items/{itemId}` e `DELETE /orders/{id}/items/{itemId}` aceitam `Idempotency-Key`, um UUID diferente por operação. Uma repetição da mesma operação usa o mesmo UUID; reutilizá-lo para outro conteúdo retorna conflito.

O HTTP 200 confirma a gravação. `erpItemSync.pending` informa se ainda falta confirmação do ERP; `processing`, `attempts` e `lastError` detalham o andamento. O checkout público omite o erro interno. O frontend consulta o pedido a cada dois segundos enquanto houver pendência.

A fila atende edições do lojista em pedidos Tiny existentes, independentes e abertos, para produtos vinculados ao Tiny sem promoção/fila de espera ativa. Pedidos agrupados, itens em fila de espera, produtos sem vínculo, outros ERPs e clientes sem a nova chave continuam pelo fluxo síncrono existente. As proteções de clique e o agrupamento de cliques no stepper também se aplicam a esses casos.

## Persistência e recuperação

A migração `000153` acrescenta `cart_erp_edits` e `cart_erp_edit_requests`; não altera pedidos existentes. Cada carrinho mantém uma revisão desejada e outra confirmada. As operações registradas identificam produtos removidos mesmo depois da exclusão da linha local, inclusive em pedidos antigos sem marcador nos itens do Tiny.

O worker verifica trabalho a cada dois segundos, com uma janela de um segundo para agrupar edições. Reivindica até cinco carrinhos, com prazo de operação de 90 segundos e lease de três minutos. Alterações adicionais no carrinho são recusadas enquanto o envio estiver em andamento. Falhas persistem o erro e a próxima tentativa; reiniciar o servidor não descarta trabalho aceito.

O worker reutiliza a máquina de estados e os limitadores existentes. A recuperação de operações ERP interrompidas também consulta os produtos das edições pendentes, inclusive quando a edição removeu o último item. Antes de reescrever, relê o pedido: se produtos, quantidades e preços já correspondem, não faz outro PUT nem um estorno desnecessário.

## Estoque, frete e pagamento

- Acréscimos reservam o saldo local atomicamente; falta de saldo recusa a alteração.
- Reduções retêm o estoque até a confirmação da grade pelo ERP. Várias edições do mesmo produto reutilizam a reserva retida; a confirmação credita somente o saldo líquido, uma vez.
- Eventos de reserva/liberação e auditoria da edição acompanham a mesma transação da mudança local.
- O espelho de estoque não sobrescreve produtos com alterações pendentes. A versão `erp_seq` invalida leituras iniciadas antes da alteração ou da liberação.
- O frete anterior é limpo na transação da edição. Selecionar novo frete, gerar cobrança, confirmar pagamento manual ou agrupar pedidos aguarda a confirmação pendente.
- Uma reflexão do pedido Tiny para o carrinho aguarda a fila local, evitando que uma leitura anterior desfaça a edição.
- Pagamento recebido durante a edição exige conferência antes de continuar a sincronização. O worker não confirma sucesso sobre essa divergência.
- Após confirmar a liberação, o fluxo existente pode promover o próximo cliente da fila.

A consulta de **estoque disponível** continua intacta. Não houve troca por estoque físico nem eliminação da leitura necessária para calcular o disponível.

## Logs

| Mensagem | O que permite medir |
| --- | --- |
| `merchant edit queued` | Revisão aceita, carrinho, loja, operação e duração da gravação |
| `merchant edit sync completed` | Revisão confirmada, tentativa, espera na fila e tempo de processamento |
| `merchant edit sync deferred` | Falha, próxima retomada persistida, tentativa e duração |
| `ERP order grid already matches; write skipped` | Escrita evitada pela releitura |
| `ERP order grid write finished` | Duração total da atualização e resultado |
| `ERP stock reversal finished` | Tempo e resultado do estorno após recusa por estoque lançado |
| `ERP order mutation wait completed` | Espera por outra mutação consolidada em um log |
| `ERP write budget wait` | Espera na fila de escrita e no orçamento da loja |
| `provider request quota wait` | Espera no limitador da integração, incluindo desistência por prazo |

O loop de espera por outra mutação passa a registrar cada tentativa somente em debug; a conclusão registra um resumo. O novo fluxo reduz escritas e mantém retentativas duráveis, mas a latência e a disponibilidade da Tiny continuam externas ao LiveCart.

## Publicação e validação

Publicar **backend com a migração primeiro**, depois o frontend. A versão anterior do frontend continua compatível; não recebe o comportamento assíncrono até enviar a chave. O rollback da migração recusa remover as tabelas se ainda houver trabalho pendente. Reverter o backend exige manter um worker desta versão até drenar a fila.

Validação local: suíte Go com detector de corridas, testes com PostgreSQL usando o mesmo modo de parâmetros da produção, build do servidor, testes de operações do frontend e `npm run build`.

Os testes cobrem repetição concorrente de uma adição, reserva insuficiente, consolidação de quantidades, remoção do último item, preservação de linhas manuais, falha e retomada após reinício, exclusão entre workers, pagamento/frete/junção bloqueados e recuperação de uma grade vazia interrompida. Os ERPs são simulados; nenhuma alteração de produção é necessária para esses testes.

Em staging, conferir uma sequência de edições em pedido de teste Tiny: resposta local com pendência, transição para concluído, grade final no ERP e estoque disponível. Durante indisponibilidade, o pedido deve manter pendência visível e retomar automaticamente. Pedido com nota fiscal ou divergência de pagamento requer conferência; a fila não remove essas regras.

## Documentação consultada

- [Tiny/Olist — atualizar itens do pedido](https://api-docs.erp.olist.com/api-reference/pedidos/atualizar-itens-do-pedido): o PUT substitui a lista completa e recalcula totais, impostos e parcelas. Essa é a base para agrupar edições e preservar linhas manuais na releitura.
- [Tiny/Olist — estornar estoque do pedido](https://api-docs.erp.olist.com/api-reference/pedidos/estornar-estoque-do-pedido): a operação específica de estorno permanece condicionada às proteções existentes de estoque lançado e nota fiscal.
- [Olist — configurações e utilização da API v3](https://ajuda.olist.com/hubs-e-plataformas-via-api/aplicativos-api-v3-configuracoes-e-utilizacao): cotas variam por plano e são compartilhadas pela conta. Não se presume o plano do cliente nem se promete ausência absoluta de 429.
- [TanStack Query — mutations](https://tanstack.com/query/latest/docs/framework/react/guides/mutations): chamadas consecutivas exigem cuidado com callbacks por chamada. O hook usa `mutateAsync` com limpeza individual em `finally` para não deixar linhas presas nem duplicar ações.
