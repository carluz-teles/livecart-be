# Tiny: pedidos na fila de atenção sem erro de finalização

## Diagnóstico em 16/09/2026

O pedido 1421, carrinho `73393e8d-95d0-4c43-9bf5-fb3e8b1740ba`, tem
finalização `done`, sem `erp_last_error`, pagamento `paid` e situação Tiny
`faturado`. A entrada em “Precisam atenção” era causada exclusivamente por
`cart_items.erp_pending_since` no produto `1097872`, duas unidades.

A marca é de 06/09 às 21:28 BRT. A quantidade confirmada era nula: é um registro
anterior ao controle de confirmação por quantidade. `RecoverPendingERPItems`
exclui essas linhas legadas, portanto a promessa de recuperação automática
exibida pelo banner não se aplica a esse caso. Não se deve remover essa guarda
e reenviar indiscriminadamente compras antigas para o ERP.

O GET da venda Tiny `848406562` (nº 27583) confirmou as duas unidades e a nota
`848575904`. O preço unitário na Tiny é R$ 116,76, enquanto o item local ainda
guardava R$ 122,90. Os demais quatro produtos já refletem a grade atual. O
lojista adicionou itens e ajustou preços na Tiny após a compra original; o
total fiscal de R$ 1.177,66 não é o mesmo fato que o pagamento original de
R$ 371,06 registrado pelo LiveCart. Não foi feita nova conciliação financeira.

Também foi confirmado o seguinte entre os pedidos sem erro de finalização:

| Pedido LiveCart | Venda Tiny | Evidência dos itens pendentes |
| --- | --- | --- |
| 1242 | 848127042 / 27331 | HX4678 presente: 1 unidade a R$ 79,90, correspondência exata |
| 1040 | 848127355 / 27358 | A4902001 ausente; venda já faturada |
| 1362 | 848323517 / 27493 | Produtos 806956156, ST6332 e CF1300 ausentes; venda aberta |
| 1403 | 848401742 / 27564 | DG1991 e A8669001 ausentes; venda já faturada |

Os três últimos continuam exigindo atenção. Não apagar a compra local nem
declarar confirmação de itens ausentes para apenas retirar o aviso. A ausência
de `erp_last_error` não comprova a sincronização dos itens: são controles
distintos da finalização do pagamento.

## Reparos locais autorizados

O usuário solicitou corrigir a classificação dos pedidos. Para o 1421,
confirmou separadamente refletir o preço da Tiny e preservar os pagamentos.

- 1242: confirmar a unidade já observada e retirar a marca antiga.
- 1421: refletir R$ 116,76 na linha de duas unidades, confirmar as duas e
  retirar a marca antiga. Preservar `paid_quantity=2` e os pagamentos.
- Incrementar `products.erp_seq` para invalidar snapshots de estoque em voo,
  sem alterar a quantidade de estoque local ou realizar movimentação ERP.

Cada reparo usa transação curta, bloqueios na ordem carrinho/produto, identidade
de loja/pedido/produto e comparação dos valores anteriores. Mudança de estado,
grade, marca, pagamento esperado ou edição em andamento aborta a transação.
GETs frescos da Tiny precedem cada execução. Evidências, backups e resultados
estão em arquivos privados fora do Git; credenciais não são persistidas neles.
Nenhuma chamada de escrita, aprovação, estorno ou criação é feita na Tiny.

## Prevenção no código

Na reflexão do pedido Tiny, a leitura de uma linha idêntica pulava diretamente
para a próxima linha. Isso deixava uma marca de falha anterior para sempre,
mesmo que uma atualização posterior confirmasse o sucesso no ERP.

Após um GET bem-sucedido e sob o bloqueio já existente de reflexão, o fluxo
passa a chamar `ConfirmERPGrid` antes de comparar os itens. O repositório
confere preço e quantidade atuais, retira apenas marcas verificadas e avança
a sequência do produto quando a confirmação muda. Uma alteração local que
ocorra durante a leitura não é confirmada por uma grade mais antiga.

Produtos repetidos na resposta ERP são excluídos da confirmação: uma linha
isolada não comprova o total de um produto dividido em várias linhas. Ausência,
preço diferente e erro de leitura conservam a pendência. O bloqueio fiscal
existente de `GetOrderItems` permanece: pedidos com nota requerem conciliação
específica, como o reparo autorizado do 1421. O novo caminho é restrito à Tiny
e reutiliza a leitura já realizada, sem chamada HTTP adicional.

## Validação

- Regressão com banco real e transporte HTTP local: as correspondências legada
  e versionada falharam antes do ajuste e passaram depois.
- Cobertura de item ausente, quantidade antiga, preço diferente, produto
  duplicado, adição concorrente, nota fiscal, falha HTTP e repetição idempotente.
- Zero escritas ERP nos testes; nenhum pagamento ou preço pendente é alterado
  automaticamente para apenas satisfazer a confirmação.
- Testes de confirmação de grade e proteção do estoque com PostgreSQL
  descartável e race detector passaram, assim como a suíte do domínio ERP e
  `go build ./apps/api/...`.
- Convenções verificadas excluindo `TestNoNewRawHttpxThrows`, falha anterior
  dos caminhos Instagram, já documentada nos incidentes precedentes.

Branch `fix/tiny-pending-item-confirmation`, baseada na main `d32d9d9`.
Não há migration nem reprocessamento geral. A prevenção precisa de deploy;
os reparos pontuais são independentes dele.
