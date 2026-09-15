# Tiny: finalização paga com estoque lançado

## Incidente confirmado em produção

Investigação em 15/09/2026, somente leitura via Railway CLI, PostgreSQL e API Tiny.
Nenhum pedido, estoque, conta a receber ou vínculo de produção foi alterado.

| Referência | Valor |
| --- | --- |
| Loja | Canto da Art |
| Pedido LiveCart | 1516 |
| Carrinho | `25807ae2-2b7c-43fa-9fbc-68e75c3a166f` |
| ID de orders | `26b6292e-88ef-48fd-ac5e-c414b09e4f67` |
| Pedido Tiny | 27830 / `848742294` |
| Operação de finalização | `d5231dc9-4461-4d27-8204-0eac6bf47dd2` |
| Pagamento confirmado | 15/09/2026 16:37:10, America/Sao_Paulo |
| Última falha | 15/09/2026 16:40:14, America/Sao_Paulo |

O journal do checkout registra produtos de R$ 192,90, desconto de R$ 9,64,
frete de R$ 13,50 e uma parcela de R$ 196,76. O GET atual da Tiny mostrou
total de R$ 192,90 e frete zerado, pedido em aberto, sem nota fiscal e sem
contas a receber. O vínculo `lc-cart-25807ae2-2b7c-43fa-9fbc-68e75c3a166f`
confere. A transportadora escolhida no LiveCart é J&T.

Quatro tentativas terminaram em `pedido bloqueado para edição: estoque lançado`.
A tarefa `ecfb930c-d8f3-4bb1-8f50-c1421de80f8d` esgotou as tentativas e foi
arquivada. Houve espera pelo limitador de chamadas, mas o erro terminal foi
o bloqueio funcional da Tiny. O journal ainda não tinha preparação concluída,
criação iniciada, pedido substituto ou cancelamento da origem.

A consulta de pedidos pagos da loja com finalização falha e esse mesmo erro
retornou somente o pedido 1516; isso não representa auditoria de todos os
outros tipos de erro. A leitura dos logs de movimentação de estoque da Tiny
retornou 403. A evidência de estoque lançado é a rejeição explícita da edição,
não o campo local `erp_stock_launched`, que estava falso.

## Causa e correção

O fluxo legado de alteração de itens já tratava estoque lançado. A finalização
do checkout pago fazia um PUT com a mesma grade para verificar bloqueios e
interrompia nesse erro. Portanto, reenfileirar sem mudar o código reproduzia
a falha antes de transportar os dados pagos para o pedido final.

O usuário autorizou a regra de estornar estoque lançado e lançá-lo novamente.
O tratamento foi acrescentado à finalização Tiny:

1. Detectar e persistir o bloqueio explícito, sem liberar o estoque da origem.
2. Revalidar propriedade do pedido, nota fiscal, contas recebidas e alterações
   de itens/despesas feitas no ERP. Preservar as restrições existentes.
3. Quando os dados comerciais exigirem substituição, criar e conferir o novo
   pedido antes de estornar estoque ou cancelar a reserva original.
4. Registrar a intenção de estorno, estornar uma vez e confirmar que a origem
   voltou a aceitar edição. Cancelar a origem somente após essas verificações.
5. Conferir o checkout final, reconstruir contas abertas quando necessário,
   aprovar e relançar o estoque no pedido final.
6. Persistir o lançamento e atualizar o vínculo e `erp_stock_launched` juntos.

Quando os dados comerciais já conferem e basta atualizar as parcelas, manter
o mesmo pedido e aplicar estorno/atualização/relançamento nele. Reservas sem
estoque lançado continuam sem lançamento automático adicional.

Os novos checkpoints ficam no JSON do journal existente; não há nova migration.
O processamento continua dentro do bloqueio do carrinho e da fila de escrita
ERP existentes. A mudança de comportamento é específica da Tiny.

## Interrupções e limites

- Salvar intenção antes de POSTs de estoque e resultado com contexto próprio
  após a resposta; preservar o journal se o processamento for interrompido.
- Se um estorno ficou incerto, o bloqueio anteriormente registrado e um PUT
  posterior bem-sucedido permitem reconhecer que o estoque foi liberado.
- Não repetir lançamento cuja resposta ou checkpoint de sucesso se perdeu:
  retornar conflito de conciliação. Não há consulta de estoque por pedido que
  comprove esse resultado com as permissões disponíveis.
- Rejeições explícitas permitem uma tentativa posterior; 5xx ou perda de
  conexão após envio permanecem ambíguos. A resposta exata de sandbox
  `400 {"mensagem":"Estoque já lançado."}` é reconhecida apenas no lançamento.
- Operações manuais simultâneas no painel da Tiny não participam do bloqueio
  distribuído do LiveCart. As releituras reduzem essa janela, mas não criam
  uma transação distribuída com o ERP.

Novos logs, com cart_id, operation_id e IDs externos:

- `tiny paid checkout stock lock detected`
- `tiny paid checkout stock reversed`
- `tiny paid checkout stock restored`

## Validação

- Reproduzido antes da correção: estoque lançado sozinho e junto de contas
  abertas interrompiam a finalização com o erro do incidente.
- Testes do provider ERP com race detector; perda de resposta/checkpoint,
  rejeição temporária, repetição, falha de vínculo, nota fiscal, recebimento,
  mudança de itens durante a operação e reserva comum.
- Testes de integração com PostgreSQL/Redis descartáveis: journal, propriedade,
  vínculo atômico, flag de estoque e fluxos ERP/eventos.
- Build `go build ./apps/api/...` e ratchets de movimentação de estoque.
- E2E real em conta Tiny **ADABYTE LTDA**, com banco local e escrita restrita
  aos registros de teste: origem `372337838` (nº 122), destino `372338088`.
  Origem com contas e estoque lançados; finalização com frete e pagamentos
  mistos passou; repetição não criou outro pedido. Estorno e relançamento
  retornaram 204. Os pedidos de teste foram cancelados e seus lançamentos
  financeiros/de estoque desfeitos ao final.
- Limite da evidência E2E: confirma respostas da API, estado comercial,
  checkpoints e vínculo local; não mede o saldo físico agregado do depósito.
- A suíte completa de convenções tem uma falha preexistente em
  `TestNoNewRawHttpxThrows` (10 chamadas na integração Instagram compartilhada),
  reproduzida também na base sem esta correção. Não foi alterada neste escopo.

## Publicação e recuperação do pedido

Branch local `fix/tiny-paid-stock-recovery`, criada da `origin/main`
`2a665545f6187d796fe9f25474180fa94094327a`, sem incorporar commits alheios de stg.
Este registro não implica commit, push, deploy ou reparo em produção.

Depois de publicar a correção, uma nova tentativa do pedido 1516 poderá
retomar o journal pendente. Como a tarefa anterior foi arquivada, ela não
voltará a executar sozinha por causa do deploy. A reexecução deve conferir
novamente o estado atual antes de qualquer escrita. A substituição já usada
para corrigir os dados comerciais pode mudar o número/ID do pedido na Tiny;
o ID do pedido no LiveCart permanece o mesmo.

## Referências oficiais consultadas

- [Estornar estoque do pedido](https://api-docs.erp.olist.com/api-reference/pedidos/estornar-estoque-do-pedido)
- [Lançar estoque do pedido](https://api-docs.erp.olist.com/api-reference/pedidos/lan%C3%A7ar-estoque-do-pedido)
- [Atualizar pedido](https://api-docs.erp.olist.com/api-reference/pedidos/atualizar-pedido)
