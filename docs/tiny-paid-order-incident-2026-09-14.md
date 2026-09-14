# Tiny: finalização dos pedidos pagos — 14/09/2026

## Situação da entrega

A correção inicial foi entregue em `stg` e na branch isolada
`fix/tiny-paid-order-sync-production`, baseada em `origin/main` (`969cb43`),
e publicada pelo usuário no merge `7b46a23` (PR #74). O ajuste adicional de
serialização descrito abaixo parte desse merge e mantém a branch de produção
sem o Pagar.me Hub presente em staging. Esta investigação não alterou pedidos,
pagamentos, contatos ou estoque de produção. As escritas externas dos testes
foram feitas exclusivamente na conta Tiny ADABYTE LTDA.

## Erro 500 no reenvio após a publicação

O botão **Tentar novamente** do pedido 1495, carrinho
`41b670ac-cf19-436f-af9a-2b3fdb782729`, falhou em 14/09 às
13:37:21 e 13:37:22 de Brasília. Os logs da versão `7b46a23` e o erro
persistido em `order_payments` confirmaram:

```text
saving Tiny checkout checkpoint: ERROR: invalid input syntax for type json (SQLSTATE 22P02)
```

A migration 155 estava aplicada. Não havia operação em
`tiny_checkout_operations` para esse carrinho: a primeira gravação falhou
antes de executar a finalização na Tiny. Portanto, esses reenvios não chegaram
a criar substituto, cancelar a reserva nem refazer títulos. O vínculo continuou
no pedido Tiny 27742 / 848620609. No recorte desde a publicação às 13:33,
somente o pedido 1495 tinha esse erro de JSON; isso não implica ausência de
outros tipos de falha nos demais pedidos.

**Causa no LiveCart:** o pool da aplicação usa `pgx.QueryExecModeSimpleProtocol`,
que codifica `[]byte` como `bytea`. As duas escritas do journal (`Save` e `Bind`)
passavam o resultado de `json.Marshal` como `[]byte` para uma coluna JSONB.
Os testes anteriores usavam o protocolo padrão do pgx, que reconhecia o tipo
JSONB do parâmetro e ocultava o defeito. Não foi rate limit nem rejeição da Tiny.

**Correção:** passar `json.RawMessage` nas duas escritas, preservando o tipo JSON
sem mudar o pool, o esquema ou as regras comerciais. Os testes de banco e o
teste opt-in com a Tiny agora constroem o repositório com `database.NewPool`,
o mesmo construtor da aplicação. Com essa configuração, dois testes reproduziram
o SQLSTATE 22P02 antes do ajuste e passaram depois, incluindo conclusão atômica
do vínculo e retomada com pagamento adicional.

Validação adicional: suíte completa de integração com `-race` passou em 30,75s;
build `go build ./apps/api/...` passou. O teste real com cartão + PIX do frete,
usando agora o pool da aplicação, passou em 149,78s: reserva fictícia
371926386 → pedido final 371926650, aprovação e contas conferidas, replay sem
nova criação. A limpeza dos próprios títulos e pedidos fictícios também concluiu.

A correção exige nova publicação do backend, mas nenhuma migration adicional.
O pedido de produção não foi reenviado por esta investigação.

## Incidente de produção

### Novo reenvio às 15:30: pedido já faturado

O deploy `e51657a` (PR #75) estava ativo. O erro de JSON não reapareceu.
As tentativas de 14/09 às 15:30 do pedido 1495 chegaram à leitura da Tiny,
mas o fluxo rejeitava toda origem com nota fiscal ou situação avançada.
Essa recusa era um erro genérico, convertido em HTTP 500.

A consulta **GET** de produção confirmou pedido **27742 / 848620609**, situação
**1 (faturada)**, nota associada **848683218**, faturamento em 14/09, produtos
R$ 245,90, frete R$ 26,08 e total R$ 271,98. O journal foi criado, com
`prepared=false`, `create_started=false`, `target=null`, sem cancelamento nem
estorno de contas. A investigação não executou reenvio ou escrita em produção.

O valor total coincidir não comprova equivalência completa: o LiveCart registra
cinco parcelas, começando em 12/10 (R$ 54,39 nas quatro primeiras e R$ 54,42 na
última); a Tiny começa em 13/10 (R$ 54,38 e quatro de R$ 54,40), com vencimentos
posteriores diferentes. A API não devolveu endereço de entrega explícito; o
endereço cadastral tem o mesmo CEP, mas complemento diferente. CPF e telefone
conferem após normalização. Não se pode declarar esse exemplo integralmente
conciliado apenas pelo total e pelo faturamento, nem atribuir os ajustes a um
operador específico sem histórico de autoria.

Por orientação do usuário, pedidos Tiny já finalizados e **comprovadamente
corretos** passam a concluir a sincronização local por consulta, sem alterar
pedido, contato, estoque, nota ou financeiro da Tiny. São conferidos vínculo,
cliente, endereço, itens, frete, desconto, total, transporte, parcelas e títulos.
Observações livres de parcelas, capitalização e espaços não são divergências
comerciais; valores, vencimentos e formas de recebimento continuam validados.
Títulos recebidos podem comprovar correspondência e nunca são estornados nessa
via. Títulos ausentes ou divergentes não comprovam conclusão.

A situação real (faturado, preparando_envio, pronto_envio, enviado ou entregue)
é preservada no vínculo e no histórico, na mesma transação do journal. Não vira
uma aprovação fictícia nem um novo pedido. Operação com criação ambígua ou
substituição já em andamento continua pelo mecanismo anterior de recuperação.
Pedido cancelado ou situação desconhecida não é aceito como venda concluída.

Conflitos verificáveis recebem HTTP 422 com `ERP_RETRY_INVALID_STATE` e os campos
que precisam de conferência. Falhas técnicas mantêm o contrato de servidor.
O frontend mostra o motivo da API e atualiza os dados após uma tentativa que
falhou também; o sucesso informa conciliação, sem afirmar que houve aprovação.
O prazo dessa chamada passa de 10 para 180 segundos para comportar a fila do ERP,
sem disparar retentativas automáticas adicionais.

### Cancelados e expirados em “Precisam atenção”

Na consulta da Canto da Art, 14 carrinhos encerrados conservavam status local
`aberto` no ERP (quatro cancelados e dez expirados). Outros três expirados tinham
itens com `erp_pending_since`, incluindo os pedidos 1419, 1399 e 1363. Esses
sinais históricos incluíam 17 casos na triagem independentemente do encerramento.
Isso não prova que os 14 pedidos ainda estejam abertos na Tiny neste momento.

O filtro compartilhado por lista e contadores passa a manter cancelados e
expirados na aba Cancelados, mesmo com erro antigo de ERP, item ou envio.
Somente uma conciliação de pagamento explicitamente pendente
(`payment_review_required`) preserva a inclusão na triagem. A mudança é de
classificação: não cancela pedidos no ERP, não apaga falhas nem movimenta estoque.

Testes novos cobrem conciliação sem nenhuma escrita externa, divergências reais,
falha local e retomada, status/histórico atômicos no pool de produção, preservação
do marcador após outra cobrança, resposta HTTP 422 versus erro técnico e
exclusão/inclusão complementar na lista e nos contadores. Nenhuma migration nova.

As quatro suítes afetadas (providers ERP, serviço ERP, integração e pedidos)
passaram com `-race`, incluindo PostgreSQL descartável no modo de produção.
O build do backend passou. O fluxo real cartão + PIX na conta de testes passou
em 149,84s: #118 / 372030444 → #119 / 372032798, com limpeza dos próprios
títulos/pedidos. Esse ensaio valida o fluxo normal; a nova via sem escritas foi
validada nos testes de contrato, proteção fiscal e persistência.

### Falhas originais anteriores à publicação

Os três exemplos da Canto da Art estavam pagos no LiveCart, mas falharam na
conferência comercial antes da aprovação no ERP:

| LiveCart | Número Tiny | ID Tiny | Pagamento em Brasília | Frete | Valor pago |
| --- | --- | --- | --- | --- | --- |
| 1487 | 27733 | 848620120 | 13/09/2026 11:47:44 | R$ 30,57 | R$ 548,04 |
| 1492 | 27739 | 848620337 | 12/09/2026 16:44:35 | R$ 15,16 | R$ 68,27 |
| 1495 | 27742 | 848620609 | 12/09/2026 16:57:49 | R$ 26,08 | R$ 271,98 |

No recorte consultado, 1471, 1479, 1469 e 1476 apresentavam a mesma divergência:
sete casos recentes. Outros sete erros antigos tinham causas diferentes,
principalmente estoque insuficiente. Os casos de 12–13/09 antecedem a queda do
banco de 14/09; o erro persistido não aponta rate limit como causa desses três
exemplos. Não houve replay ou reparo desses registros em produção.

## Causas confirmadas na API

- `PUT /pedidos/{id}` devolve 204, mas ignora cliente, endereço de entrega,
  frete e desconto. A reserva de teste #97 / 371890286 permaneceu em R$ 49,90.
  O POST completo #98 / 371890340 gravou frete R$ 18,59, desconto R$ 2,50 e
  total R$ 65,99. Atualizar o contato refletiu seu cadastro, mas não o endereço
  de entrega nem o frete da reserva.
- Alterar parcelas do pedido não altera títulos já lançados. No teste #99,
  o pedido passou a duas parcelas e `GET /contas-receber?idVenda=...` continuou
  com um título de R$ 115,89 e vencimento antigo.
- O PUT não troca a forma de recebimento do pedido. O POST aceita a forma
  correspondente às parcelas. Foram validados Pix, cartão e pagamentos mistos.
- O caminho de checkout anterior representava cada cobrança do ledger como
  uma parcela, perdendo a divisão do cartão.
- `numeroOrdemCompra` é truncado silenciosamente em 50 caracteres; o novo
  marcador `lc-cart-paid-<UUID da operação>` tem 49. O UUID do carrinho permanece
  nas observações e no vínculo local durável.
- O GET de pedido devolve `numeroPedido` numérico, enquanto a resposta de criação
  o devolve como texto. GET de recebimentos vazios pode responder 204. `/info`
  devolve os dados da empresa na raiz. Os três contratos foram ajustados.

## Implementação

A finalização Tiny lê o checkout e o ledger, valida a loja, os itens vinculados
e a soma de produtos + frete cobrado − desconto. Usa a programação existente do
cartão, com arredondamento de centavos e data de liberação quando disponível;
frete pago depois por PIX vira outro recebimento na mesma compra. Pagamentos
legados sem identificação confiável do parcelamento permanecem para conciliação;
o código não inventa a quantidade ou as datas das parcelas.

Quando os dados finais exigem substituição, cria um pedido completo e confere
cliente, endereço, itens, frete, desconto, total, formas de recebimento e forma
de envio **antes de cancelar a reserva antiga**. Preserva depósito, natureza da
operação, lista de preço e vendedor, quando presentes. Não descarta silenciosamente
uma transportadora recusada pela Tiny. O custo real da etiqueta continua separado
do frete cobrado do comprador.

**O número e o ID do pedido na Tiny podem mudar.** O LiveCart atualiza o vínculo
do carrinho e de `order_payments` na mesma transação que conclui a operação.
A origem fica nas observações do pedido final e no registro local da operação.
As linhas ou despesas extras do lojista que seriam perdidas impedem a troca.

A reserva original só é cancelada depois que a nova existe. Na conta de testes,
foi possível reservar 10 unidades com as 10 já reservadas: o disponível ficou
negativo temporariamente e voltou ao correto após cancelar a origem. Não há
intervalo de liberação de estoque para outra venda. Contas com configuração que
recuse essa reserva temporária retornam erro preservando a origem; não existe
fallback de cancelar primeiro.

Contas a receber são consultadas separadamente do pedido. Quando precisam ser
refeitas, cada título deve estar aberto/vencido, com saldo integral e sem
recebimentos. O fluxo estorna os títulos antigos, verifica sua remoção e relança
a programação correta, conferindo valores e vencimentos. Título lançado não
significa título recebido. Nenhuma cobrança ou estorno de gateway é executado.

A via que altera a Tiny bloqueia pedidos com nota fiscal, estoque lançado,
recebimento registrado ou estado incompatível. Relê essas condições antes das etapas
sensíveis. As chamadas da Tiny não formam uma transação: alteração simultânea
pelo lojista ainda pode interromper o fluxo e exigir conciliação. Não se promete
reparo externo automático de pedidos faturados ou com títulos já recebidos;
quando já correspondem ao checkout, a nova via de consulta conclui apenas o vínculo local.

## Recuperação e limites

A migration **000155** cria `tiny_checkout_operations`, com uma operação ativa
por carrinho e índices para consulta por integração/carrinho. Guarda o pedido de
negócio e os checkpoints, sem tokens nem logs HTTP. Não recria `integration_logs`,
não apaga vendas existentes e não executa reparos históricos.

O checkpoint precede o POST não idempotente. Resposta perdida/5xx exige localizar
o marcador, com busca paginada a partir da data de início; falha de leitura não
vira ausência. Se não for possível provar o resultado, a criação não é repetida.
Rejeição explícita ou falha comprovadamente anterior ao envio permite nova
tentativa. O estado dessa rejeição é salvo mesmo após cancelamento do contexto.

O transporte compartilhado apenas passa a identificar erros anteriores ao envio,
preservando sua mensagem e a cadeia `errors.Is/As`. A regra que interpreta essa
informação para substituir reservas é específica da Tiny. O prazo do evento
`cart.paid` passa de 15 para 90 segundos, como `order.paid`, para comportar
pagamentos adicionais sob o limitador. Não foram alteradas regras comerciais
de Bling, gateway ou Nuvemshop.

A conta de testes informou 30 requisições/minuto, com contadores de leitura e
escrita separados. Os testes reais espaçaram requisições em 3,1 segundos. 429,
quota antes do envio e cancelamento durante a espera foram simulados localmente;
não foi provocado 429 na conta real. O orçamento/fila existente continua ativo.

Pagamentos novos durante uma operação interrompida não mudam o POST registrado:
primeiro a operação antiga é retomada, depois se aplica outra revisão. A retomada
de uma reserva já cancelada exige uma operação registrada. Um cancelamento
avulso do lojista não autoriza criar outro pedido automaticamente.

## Evidência de validação

- Teste completo com serviço ERP real, banco PostgreSQL descartável e API Tiny:
  reserva #110 / **371906633** → final #111 / **371906861**, cartão em duas
  parcelas, cliente/endereço/frete/desconto, contas refeitas, aprovação confirmada
  e replay sem nova criação. Passou em 140,50 segundos.
- Mesmo teste com **cartão + PIX do frete**: reserva #114 / **371910480** → final
  #115 / **371910707**. Total R$ 65,99, duas parcelas de cartão de R$ 23,70 e PIX
  de R$ 18,59, Correios conferido por ID. Passou em 149,81 segundos.
- Após cada teste, seus próprios títulos foram estornados e os pedidos fictícios
  cancelados. Os fixtures #97 e #98 permanecem abertos, reservando duas unidades
  do produto de testes 371890254. Não houve cobrança de gateway nem nota fiscal.
- Testes locais cobrem resposta perdida na criação/cancelamento, falha no vínculo
  do banco, recuperação com pagamento posterior, marcador de até 50 caracteres,
  isolamento por loja, exclusão mútua, títulos parcialmente recebidos, recibo com
  saldo integral, estoque+contas lançados, nota fiscal antes/durante a operação,
  erro de leitura financeira, cartão parcelado e preservação de saldo a pagar.
- Suítes ERP, integração, todos os providers e eventos passaram com `-race`.
  Os testes de integração aplicam as migrations em um banco descartável.
  Build do backend passou. O checkout pela interface e emissão fiscal não foram
  homologados nesta rodada; o teste real executa diretamente o serviço ERP.

Reprodução opt-in: `go test -tags=tiny_checkout_e2e
./apps/api/internal/integration -run '^TestE2ETinyFinalizationDatabase$' -v`.
Exige `TEST_DATABASE_URL` local, token da conta de testes, fixture validado e
`TINY_E2E_MIXED_PAYMENTS=1` para pagamentos mistos. O transporte de teste restringe
as escritas aos pedidos que ele próprio criou e ao produto/contato fictícios.
Credenciais e artefatos privados não pertencem ao Git.

## Acompanhamento após publicação

Logs: `tiny paid checkout reconciliation started` indica início/retomada;
`tiny paid checkout reconciled` confirma vínculo concluído, com `cart_id`,
`operation_id`, `source_order_id`, `target_order_id`, `replacement` e
`receivables_rebuilt`. Falhas continuam visíveis na finalização existente.

Publicar o código não equivale a corrigir todos os pedidos antigos. Os exemplos
1487/1492/1495 e os demais precisam de nova leitura do estado fiscal/financeiro e
autorização específica antes de qualquer alteração em produção.

## Referências oficiais usadas

- [Criar pedido](https://api-docs.erp.olist.com/api-reference/pedidos/criar-pedido)
- [Atualizar pedido](https://api-docs.erp.olist.com/api-reference/pedidos/atualizar-pedido)
- [Obter pedido](https://api-docs.erp.olist.com/api-reference/pedidos/obter-pedido)
- [Listar contas a receber](https://api-docs.erp.olist.com/api-reference/contas-a-receber/listar-contas-a-receber)
- [OpenAPI v3](https://erp.olist.com/public-api/v3/swagger/swagger-mintlify.json)

As limitações não descritas no contrato, como truncamento e descarte silencioso,
foram confirmadas nos ensaios acima; não são inferidas apenas da documentação.
