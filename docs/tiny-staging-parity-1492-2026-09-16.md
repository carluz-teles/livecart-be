# Tiny: conciliação do pedido 1492 e diferenças entre stg/main

## Evidência em produção, somente leitura

- Carrinho `5812c8dd-3876-4e14-b9ea-5e6252017e7e`, pedido interno nº 1492.
- Tiny nº 27739, ID `848620337`, faturado, nota fiscal `848726030`.
- Produção na main `42185f1`, deployment
  `cc5bdc6e-5259-46d7-b1ea-3d772ae5de8b`, concluído em 16/09 às 12:33 BRT.
- Nova tentativa às 12:36:39 BRT retornou HTTP 422, com
  `pedido Tiny 848620337 (faturado) exige conciliação: confira desconto`.
- Produtos R$ 55,90, frete R$ 15,16 e total pago R$ 68,27 conferem.
- A API Tiny retorna desconto `2.795`, despesa `0.01` e total `68.27`.
  O LiveCart registra desconto de 279 centavos. A comparação estrita do
  desconto arredondava o valor Tiny para 280 centavos e bloqueava o vínculo.
- Parcela PIX de R$ 68,27 em 12/09; uma conta a receber de mesmo valor,
  em aberto, vencimento 14/09. Contas lançadas não significam recebimento.
- O journal permanece sem preparação, criação, cancelamento ou estorno
  iniciados. A falha está na conferência, antes de qualquer escrita ERP.

O pedido 1487 foi consultado também: Tiny `848620120`, faturado, desconto
`27.235000000000003`, despesa `0.01`, frete R$ 30,57, total R$ 548,04.
É o mesmo padrão de compensação; o LiveCart espera desconto de 2723 centavos.

Foram usados Railway CLI, PostgreSQL em transação READ ONLY e GETs da Tiny.
Não houve refresh OAuth, retry de produção, escrita no ERP ou reparo no banco.
Respostas privadas e dados pessoais não foram adicionados ao repositório.

## Causa da recorrência e escopo do envio

O tratamento da compensação e do reaproveitamento já estava em stg no commit
`1896684`, de 14/09. Não foi incluído nos PRs isolados que chegaram à main.
O deploy mais recente continha a correção do calendário de pedidos faturados,
mas ainda não continha esses ajustes anteriores. Esta foi uma falha na seleção
das alterações para produção, além da comparação originalmente rígida.

A branch `fix/tiny-reconciliation-staging-parity` parte da main `42185f1` e
inclui as diferenças pendentes da finalização Tiny em stg:

1. Reconhecer a compensação de exatamente um centavo em despesas quando o
   desconto compensado e o total pago conferem. O snapshot ERP não é editado;
   divergências de total, despesas maiores e valores sem compensação bloqueiam.
2. Reaproveitar pedidos corrigidos manualmente com grade, cliente, endereço,
   frete, pagamento e financeiro conferidos, antes de substituir a reserva ou
   estornar lançamentos. Preservar o vencimento da conta PIX conforme a regra
   aprovada pelo usuário. Se já faturado, apenas ler e concluir o vínculo local;
   se ainda aberto e sem nota, aprovar e conferir novamente. Não reaproveitar
   uma origem quando substituição, cancelamento ou estornos já começaram.
3. Tratar 404 da venda paga como conflito de conciliação, com mensagem de
   pedido não encontrado. Não recriar automaticamente uma venda paga ausente.
   Erros de autenticação ou de servidor não são convertidos em ausência.

Arquivos de execução transportados: `erp/tiny_checkout.go`,
`erp/tiny_finalization.go`, `erp/tiny_reconcile_existing.go` e
`providers/tiny_checkout.go`, dentro de `internal/integration/providers`.
Os demais arquivos Go modificados são testes Tiny ou da resposta HTTP ao retry.
As mudanças de Pagar.me Hub, configuração e handlers de pagamentos presentes
em stg foram examinadas e excluídas. Não há migration neste envio.

## Validação

- Suíte do provider ERP com race detector: passou.
- Testes Tiny de integração e retry HTTP com PostgreSQL descartável e race
  detector: passaram. Build `go build ./apps/api/...`: passou.
- Convenções passaram excluindo `TestNoNewRawHttpxThrows`, falha preexistente
  dos caminhos Instagram, já documentada nos incidentes anteriores.
- Novo teste cobre pedidos faturados nos formatos 1492/1487, falha no vínculo
  local, retomada e repetição: preserva nota, venda, estoque e contas, com zero
  escritas ERP e o mesmo ID/status faturado.
- Replay local de cópias privadas das respostas reais dos pedidos 1492, 1487
  e 1495 passou pelo fluxo completo `FinalizePaidCheckout`. O transporte local
  admitia somente GETs: zero escritas ERP; repetição concluída não fez chamadas.
  O 1495 protege a conciliação de parcelas consolidadas da correção anterior.
- Testes negativos preservam bloqueios de total divergente, despesas distintas,
  títulos ausentes/duplicados/inválidos e venda ausente. Fixtures privadas e o
  teste temporário de replay não fazem parte do commit.

## Depois do deploy

Repetir a tentativa do 1492 deve reler o ERP e concluir com o mesmo pedido
faturado se os dados continuarem como os observados. O mesmo vale para o 1487.
O log `tiny existing paid checkout reconciled` inclui
`rounding_adjustment_preserved`, `receivable_dates_preserved`,
`preserved_financial_schedule`, `approved_existing_order` e `has_invoice`.

A consulta encontrou 13 pedidos pagos com estado de falha persistido, incluindo
erros históricos de estoque, dados de checkout e venda excluída. Essa contagem
não representa 13 novas falhas do deploy atual, nem comprova que todos serão
resolvidos por esta versão. Não foi feito reprocessamento em massa; casos com
divergências reais ou venda ausente continuam exigindo conferência específica.
