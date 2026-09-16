# Tiny: pedido 1495 faturado e ajustado manualmente

## Consulta em produção, somente leitura

- Carrinho: `41b670ac-cf19-436f-af9a-2b3fdb782729`.
- Pedido interno: `a0ee2ae0-43af-4c7e-9486-e357602f9c4b`, nº 1495.
- Tiny: nº 27742, ID `848620609`, situação 1 (faturado).
- Nota fiscal: nº 021101, ID `848683218`, situação 7 retornada pela API.
- Journal: `26f18a12-ecb0-4e61-a592-984fb1271a3c`, não preparado, sem
  substituição, cancelamento, estorno ou destino iniciado.
- Versão em produção: `c48ffc8`, deployment
  `e9d3bd32-80e3-4fb3-be09-b41fa62f3876`, sucesso em 16/09 às 11:44 BRT.
- Tentativas em 16/09 às 12:04:46 e 12:04:50 BRT retornaram HTTP 422,
  `ERP_RETRY_INVALID_STATE`. Não houve HTTP 500 nessas tentativas.

A análise usou Railway CLI, consultas PostgreSQL em transação `READ ONLY` e
GETs da Tiny. Nenhuma nova tentativa, edição, conciliação local ou alteração
de dados de produção foi executada.

## Diferenças confirmadas

O total do pedido, da nota, das cinco parcelas da Tiny e da conta a receber
é R$ 271,98; o LiveCart registra o mesmo valor pago, cartão em cinco parcelas.
O frete confere em R$ 26,08. O envio é Jadlog via Smart Envios.

| Parcela | LiveCart | Tiny | Data LiveCart | Data Tiny |
| --- | --- | --- | --- | --- |
| 1 | R$ 54,39 | R$ 54,38 | 12/10/2026 | 13/10/2026 |
| 2 | R$ 54,39 | R$ 54,40 | 11/11/2026 | 13/11/2026 |
| 3 | R$ 54,39 | R$ 54,40 | 11/12/2026 | 14/12/2026 |
| 4 | R$ 54,39 | R$ 54,40 | 10/01/2027 | 14/01/2027 |
| 5 | R$ 54,42 | R$ 54,40 | 09/02/2027 | 14/02/2027 |

A Tiny tem uma única conta a receber, ID `848683038`, valor e saldo
R$ 271,98, vencimento 28/09/2026, situação `aberto`. Essa conta não representa
cinco títulos idênticos às parcelas previstas pelo LiveCart. Conta lançada
não é evidência de baixa/recebimento: a consulta retornou saldo integral.

O endereço separado está nulo. O endereço do cliente confere em rua, número,
bairro, cidade, estado e CEP. No complemento foi acrescentada uma referência
à empresa, preservando o andar informado. Os dados completos de endereço
e as respostas privadas não foram adicionados ao repositório.

## Por que permanece pendente

A correção de 16/09 reconhece endereço do cliente quando o endereço separado
está nulo. Ela preserva a comparação do complemento e do financeiro. Neste
pedido há diferenças reais de representação, não ausência de dados enviada
pela API. A validação atual exige os mesmos valores/datas por parcela e um
título correspondente a cada parcela. O código já tem uma regressão desse
mesmo calendário, que explicitamente espera bloqueio.

O erro atual agrega três divergências: endereço de entrega, parcelas/formas
de pagamento e contas a receber. O journal não conclui o vínculo, de modo que
o LiveCart mantém `erp_finalisation_status=failed` e o estado anterior do ERP.

O teste de regressão financeira e os testes de endereço/conciliacão de pedido
faturado passaram na versão atual. Isso reproduz a regra vigente; não significa
que a regra atenda à aceitação de ajustes manuais solicitada pelo lojista.

## Regra aprovada e correção

O usuário confirmou: preservar o financeiro da Tiny nessas condições. A regra
foi implementada na conciliação de pedidos existentes com nota fiscal e situação
faturado ou de expedição posterior. Reservas e pedidos substitutos continuam
sujeitos à comparação estrita usada nas escritas automáticas.

- Manter identidade, grade de produtos, total, desconto, frete e transportadora.
- Conferir quantidade e soma em centavos **por forma de pagamento**. É permitido
  preservar as datas e a distribuição entre parcelas do mesmo meio; não é
  permitido transferir valores entre cartão/PIX ou alterar a quantidade deles.
- Aceitar títulos consolidados com IDs distintos, situação reconhecida,
  valores positivos, saldo entre zero e valor, datas válidas e total exato.
  Títulos ausentes, cancelados, duplicados ou com valores divergentes bloqueiam.
- Reler o pedido e as contas antes de concluir a sincronização local.
- Reconhecer uma referência expressamente identificada (`empresa` ou
  `referência`) acrescentada entre parênteses depois do complemento original.
  O complemento original deve permanecer completo; rua, número, unidade,
  bairro, cidade, estado e CEP continuam verificados. Um destino explícito
  diferente não é substituído pelo endereço do cadastro.
- Preservar o snapshot original do pagamento no journal e registrar as decisões
  `PreservedFinancialSchedule` e `PreservedDeliveryReference` no JSON existente,
  sem migration e sem armazenar novas respostas integrais do ERP.

A conciliação usa apenas GETs da Tiny, preserva a nota e os lançamentos e
conclui a sincronização local com o status fiscal observado. Não recria o
pedido nem altera contato, parcelas, contas ou estoque. O log existente
`tiny existing paid checkout reconciled without ERP writes` passa a conter
`preserved_financial_schedule`, `preserved_delivery_reference`,
`installment_count` e `receivable_count`, sem dados pessoais.

## Validação e publicação

- Reprodução anônima das cinco parcelas, conta única e referência de endereço:
  falhou antes da correção e passou depois.
- Testes de recebimento integral/parcial, consolidação, métodos mistos, valores,
  datas inválidas, mudança de contas durante a conferência, endereço diferente,
  repetição e falha de vínculo local. Nenhuma escrita ERP nessas conciliações.
- Replay local de cópias privadas das respostas reais do pedido 1495: reconheceu
  o mesmo pedido como faturado e registrou ambas as decisões de preservação;
  repetição não fez novas chamadas. Fixtures privadas não estão no repositório.
- Teste com provider real, transporte HTTP simulado e PostgreSQL descartável:
  removeu `erp_last_error`, concluiu `erp_finalisation_status=done`, preservou o
  vínculo e gravou `faturado`; manteve o calendário original do pagamento local.
- Suíte ERP com race detector, testes Tiny/HTTP com banco e build passaram.
- Convenções passaram excluindo `TestNoNewRawHttpxThrows`, falha preexistente
  na integração Instagram já documentada nos incidentes anteriores.

Branch isolada `fix/tiny-invoiced-manual-reconciliation`, baseada na main
`c48ffc8`. Nenhum pedido de produção foi reprocessado. Depois do deploy, uma
nova tentativa do 1495 deverá reler o ERP e concluir se os dados permanecerem
como os conferidos. Os testes deste ajuste não emitiram nota fiscal na conta
demo; a evidência fiscal veio de consultas e do replay local sem escritas.

## Referências consultadas

- [Obter pedido](https://api-docs.erp.olist.com/api-reference/pedidos/obter-pedido)
- [Obter nota fiscal](https://api-docs.erp.olist.com/api-reference/notas/obter-nota-fiscal)
- [Listar contas a receber](https://api-docs.erp.olist.com/api-reference/contas-a-receber/listar-contas-a-receber)
