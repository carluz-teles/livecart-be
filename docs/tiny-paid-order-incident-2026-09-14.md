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

### Reenvios das 17:56: versão pendente e pedido 1479 não localizado

Às 20:57 UTC, a Railway continuava executando o backend `92e64b1` (PR #77),
deploy `6a6fec85-a7bc-4426-8095-9a2bb555debf`, criado às 19:31 UTC. O commit
`e56f8c1`, que reaproveita as reservas com arredondamento e preserva o vencimento
do título PIX, estava em staging e na branch de correção, mas ainda fora de
`origin/main`. O usuário confirmou expressamente a preservação do vencimento
da Tiny quando o restante confere.

Os logs e as últimas tentativas em `order_payments` identificaram:

| Pedido | Tentativa UTC | Resultado |
| --- | --- | --- |
| 1487 | 20:56:10 | 422 por despesas adicionais; correção `e56f8c1` ainda não publicada |
| 1495 | 20:56:28 | 422 por endereço, parcelas e contas a receber divergentes |
| 1479 | 20:56:43 e 20:56:44 | 500 causado por GET Tiny retornando 404 |

O 1479, carrinho `fc6777e4-5767-4b2b-a544-e2e58608399d`, continua vinculado ao
pedido Tiny **27722 / 848619303**. O pagamento local ocorreu em 12/09 às
18:31:31 UTC. A consulta direta do pedido retornou 404; buscas somente por
leitura com `numero=27722` e pelo documento cadastrado da cliente retornaram
zero resultados. Não há evidência suficiente para atribuir exclusão a uma
pessoa, identificar outro pedido como substituto ou criar uma nova venda.
Foi solicitada ao usuário a conferência no painel da Tiny.

O journal `fa0fe45c-2661-4524-9550-48592c500773` permanecia sem preparação,
criação iniciada, pedido substituto, cancelamento ou estorno. Essas tentativas
pararam na consulta da origem.

**Correção adicional:** o GET 404 do checkout passa a produzir o conflito Tiny
tipado, com mensagem de pedido não encontrado e HTTP 422. A origem ausente não
entra na conciliação de um objeto nulo nem no fluxo legado que recria reservas
não pagas. O mesmo tratamento cobre substituto ausente antes e depois do
cancelamento da origem. Erros de autorização e falhas do servidor continuam
sendo erros técnicos; não são interpretados como ausência da venda.

Os testes exercitam duas tentativas por estágio, sem escrita no ERP, troca de
vínculo ou descarte do checkpoint, e verificam o contrato HTTP. Não há migration,
alteração de frontend ou escrita em produção. Publicar o pacote resolve o
tratamento do erro; recuperar o 1479 exige esclarecer o vínculo com a Tiny.

### Reenvios das 16:33 após o PR #77: concluir os pedidos existentes

O deploy `92e64b1` estava ativo. O pedido 1492 foi reenviado às 19:33:02 UTC;
o 1487 (`cf5623d6-3ecf-4a0c-a98c-642a87e55086`) às 19:33:18 e 19:33:22 UTC.
Ambos retornaram **422**, com `despesas adicionais do pedido`. A correção do
HTTP 500 havia sido publicada, mas a regra continuava impedindo a conclusão.

A comparação completa confirmou, nos dois exemplos, correspondência de nome,
documento, email, telefone, endereço cadastral, produtos, quantidades, preços,
transportadora, frete e parcela PIX da venda. O impedimento era a representação
do desconto/arredondamento e a exigência de que vencimento financeiro fosse
idêntico à data do PIX:

| Campo | 1492 / Tiny 848620337 | 1487 / Tiny 848620120 |
| --- | --- | --- |
| Produtos | R$ 55,90 | R$ 544,70 |
| Frete | R$ 15,16 | R$ 30,57 |
| Total Tiny = pago LiveCart | R$ 68,27 | R$ 548,04 |
| Desconto bruto retornado pela Tiny | 2.795 | 27.235000000000003 |
| Outras despesas Tiny | 0.01 | 0.01 |
| Desconto líquido LiveCart | 279 centavos | 2723 centavos |
| Parcela PIX na venda e no LiveCart | 12/09, R$ 68,27 | 13/09, R$ 548,04 |
| Título a receber | 848668883, aberto, 14/09 | 848670098, aberto, 14/09 |

O centavo compensa a diferença de arredondamento do desconto, preservando
exatamente o total pago. Não é justificativa para excluir uma linha financeira
ou criar outro pedido. A comparação anterior do desconto isolado foi restritiva
demais para esses casos. O vencimento de uma conta a receber é um campo separado
da parcela que registra o PIX; sua alteração é documentada em
[Atualizar conta a receber](https://api-docs.erp.olist.com/api-reference/contas-a-receber/atualizar-conta-a-receber).

**Regra implementada:**

- Antes de substituir uma reserva, conferir se a venda já está completa.
- Aceitar, somente nessa conferência, despesa de exatamente um centavo que
  compense o desconto arredondado, com desconto líquido e total pago idênticos.
  Não existe tolerância de um centavo no total. Outros ajustes permanecem
  protegidos; o caminho de criação continua exigindo os valores enviados.
- Preservar o vencimento de um único título PIX com ID, valor e situação
  verificáveis. A parcela da venda continua exigindo data, valor e método do
  pagamento. Parcelamento de cartão, pagamentos mistos, título ausente,
  duplicado, cancelado ou com valor diferente não recebem essa exceção.
- Para uma venda correta ainda aberta e sem nota, enviar apenas a aprovação
  e reler pedido/financeiro antes do vínculo local. Se já estiver aprovada ou
  faturada, conciliar mantendo a situação real, sem escrita na Tiny.
- Não cancelar/recriar pedido, estornar estoque/contas, lançar recebimento ou
  reescrever contato, entrega, frete e parcelas nesse caminho. Uma criação
  anterior de resultado incerto impede a adoção da origem.

Logs de sucesso: `tiny existing paid checkout reconciled`, com `cart_id`,
`order_id`, `erp_status`, `approved_existing_order`,
`receivable_dates_preserved` e `rounding_adjustment_preserved`.

Os dois cenários de arredondamento foram reproduzidos localmente: falhavam na
comparação antiga e passam na nova. Os testes cobrem estoque bloqueado, retomada
de falha no vínculo local, replay sem duplicação e preservação dos valores e
IDs financeiros. O pedido 1495 continua fora dessa exceção: tem complemento e
parcelamento de cartão divergentes, além de um título único para cinco parcelas.

Homologação real na conta ADABYTE: pedido fictício **#121 / 372067576**, PIX
R$ 65,99, estoque e contas previamente lançados e título **372067604** com
vencimento diferente do dia do PIX. O serviço completo com PostgreSQL local
concluiu a finalização usando o mesmo ID e gravou `erp_finalisation_status=done`.
A única escrita da conciliação foi `PUT /pedidos/372067576/situacao`; quantidade,
ID, valor, saldo e vencimento do título permaneceram iguais, assim como estoque
físico, reservado e disponível. O replay não criou outro pedido. Passou em
72,30s; depois o teste limpou apenas suas próprias contas, estoque e pedido.
O arredondamento fracionado é coberto pelas reproduções locais dos retornos de
produção; o teste real valida a aprovação sem alterar estoque/financeiro.

Reprodução adicional: executar o teste opt-in abaixo com
`TINY_E2E_EXISTING_PIX=1`. As suítes de providers ERP, serviço ERP, integração e
pedidos passaram com `-race`; o build do backend também passou.

As leituras dos pedidos de produção foram somente GET/SELECT. Nenhum deles foi
reenviado ou alterado pela investigação. Não há migration ou mudança de frontend.

### Reenvios das 16:15 após o PR #76: pedidos 1495 e 1492

Backend `8c10d8f` e frontend `10e1eec` estavam publicados com sucesso.
As tentativas de 14/09/2026 às 19:15 UTC foram identificadas pelos logs e por
`erp_last_attempt_at`, sem executar retry ou escrita em produção:

| Pedido LiveCart | Pedido Tiny | HTTP | Causa imediata |
| --- | --- | --- | --- |
| 1495 | 27742 / 848620609 | 422 | Endereço, parcelas e forma de envio divergentes na comparação |
| 1492 | 27739 / 848620337 | 500 | Despesas adicionais do ERP bloqueiam substituição; erro ainda genérico |

O journal de ambos continuou sem preparação, criação, cancelamento ou estorno.
O erro de serialização JSON não retornou. Nenhuma das consultas constatou
duplicação de pedido por essas tentativas.

**Defeitos corrigidos nesta rodada:**

- Ajustes protegidos de despesas/itens retornam o erro de conciliação tipado,
  com campo identificável e HTTP 422, em vez de erro interno 500.
- Na conciliação somente por leitura, `enderecoEntrega=null` usa o endereço
  do cliente. Endereço de entrega explícito, mesmo incompleto, continua tendo
  prioridade; não se ocultam complemento ou destino diferentes. A verificação
  estrita do pedido substituto não recebeu esse fallback.
- A mesma transportadora pode ter cadastro direto e via Smart Envios. A Tiny
  retornou Jadlog direto `774610201` e Jadlog via Smart Envios `842253615`; o
  pedido 1495 usa o segundo. Para Loggi, a listagem também tem vários IDs,
  incluindo `842253618`, usado no 1492. A conferência de um pedido existente
  aceita nome exato da transportadora ou sua variante `via Smart Envios`, sem
  aceitar outra transportadora por substring. A criação continua verificando
  o ID solicitado.
- A conciliação informa divergências comerciais e de contas a receber juntas,
  evitando que uma nova tentativa apenas revele a próxima divergência.

**Diferenças encontradas nessa leitura (o arredondamento/PIX do 1492 foi
reavaliado na seção das 16:33; os valores não devem ser apagados):**

- 1492: situação Tiny `0` (aberto), sem nota vinculada. Produtos R$ 55,90,
  frete R$ 15,16 e total R$ 68,27 conferem. A API retorna desconto `2.795`
  e outras despesas `0.01`; o LiveCart espera desconto de 279 centavos.
  O endereço cadastral confere por completo. A parcela PIX da venda vence em
  12/09, mas o título `848668883` de R$ 68,27 vence em 14/09 e está aberto.
- 1495: segue faturado. O complemento cadastral difere do checkout; valores
  e vencimentos das cinco parcelas diferem conforme detalhado abaixo.
  Além disso, `/contas-receber?idVenda=848620609` retorna um único título,
  `848683038`, de R$ 271,98, aberto, vencendo em 28/09, enquanto o LiveCart
  registra cinco parcelas. Portanto, o total pago igual não comprova que
  financeiro, entrega e sincronização estejam conciliados.

Não se atribuem essas mudanças a uma pessoa sem histórico de autoria. Esta
rodada não marca nenhum desses pedidos como concluído nem altera seus valores.
As credenciais, endereços e respostas completas ficaram em artefatos privados.

Validação: reproduções locais dos formatos de resposta dos dois incidentes,
testes de comparação de endereço e múltiplos cadastros da transportadora,
conflitos financeiros reportados em conjunto e contrato HTTP 422/500. Os testes
verificam zero escritas na Tiny para os conflitos e conciliações por leitura.
Nenhuma migration ou alteração de frontend é necessária nesta rodada.

Referências: [consulta do pedido na API v3](https://api-docs.erp.olist.com/api-reference/pedidos/obter-pedido)
e [separação de endereço cadastral e entrega na integração Olist](https://ajuda.olist.com/plataformas-de-e-commerce/integracao-erp-da-olist-com-xtech).
A documentação descreve endereços separados para destinos diferentes; a resposta
real confirma `enderecoEntrega=null` nestes dois pedidos. Os múltiplos cadastros
de transportadora foram comprovados por GET de `/formas-envio` da própria loja.

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
