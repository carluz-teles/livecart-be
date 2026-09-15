# Tiny: CPF já cadastrado na finalização do checkout

## Incidente confirmado

Inspeção em produção somente leitura, pela Railway CLI, PostgreSQL e API Tiny.

- Canto da Art, pedido LiveCart **1516**.
- Carrinho `25807ae2-2b7c-43fa-9fbc-68e75c3a166f`.
- Tiny **27830**, ID `848742294`.
- Journal `d5231dc9-4461-4d27-8204-0eac6bf47dd2`.
- Tentativas em 15/09/2026, 18:59:56 e 19:00:00, America/Sao_Paulo;
  última falha persistida às 19:00:02, sexta tentativa registrada.

Após o deploy da correção de estoque (`2557d1c`, incorporado na main
`62f0493`), a finalização avançou até a atualização do comprador e recebeu
HTTP 400: contato com o documento já existe. O CPF não é reproduzido neste
relatório nem no novo log de resolução.

As leituras confirmaram dois contatos diferentes no mesmo ERP:

| Cadastro | ID Tiny | Documento | Situação |
| --- | --- | --- | --- |
| Reserva, `daninicruz` | `811688006` | Não preenchido | B, ativo |
| Compradora, Daniela Cruz | `809619618` | Confere com o checkout | A, ativo com acesso ao sistema |

A busca por `cpfCnpj` formatado retornou exatamente um contato, o segundo.
O código anterior atualizava sempre o ID da reserva com os dados do checkout,
sem resolver a identidade fiscal já existente. Isso causava a duplicidade.
O teste anterior de estoque usava o mesmo contato na reserva e no checkout;
portanto não cobria este cenário de dois cadastros.

O checkpoint estava `Prepared=true`, `Replace=true`, `SourceStockLaunched=true`,
mas `CreateStarted=false`, sem destino, cancelamento, estorno de estoque ou
estorno de contas. O GET do pedido continuava em aberto, sem nota fiscal,
frete zero, total R$ 192,90 e sem contas a receber. Nenhuma recuperação manual
foi executada em produção nesta investigação.

## Correção

A resolução foi adicionada exclusivamente à finalização de checkout pago da
Tiny, antes de eventual estorno financeiro e criação do pedido substituto:

1. Buscar pelo CPF/CNPJ formatado, sem filtro por nome ou usuário do Instagram.
2. Conferir o documento, ID, unicidade e situação ativa na listagem. Uma lista
   incompleta, uma resposta inválida ou erro HTTP interrompe a operação.
3. Reler o cadastro escolhido e conferir novamente sua identidade/situação.
4. Persistir o novo ID no journal antes de avançar com as operações do pedido.
5. Atualizar os dados do contato resolvido e prosseguir com o fluxo existente.

Se a busca não encontra cadastro, reler o contato da reserva e somente permitir
seu enriquecimento quando o documento estiver vazio ou já for o do comprador.
Um documento diferente é preservado e exige conciliação. Contatos inativos,
excluídos ou resultados ambíguos também exigem conferência, sem fusão/exclusão
ou criação automática de contatos.

Checkpoints antigos são resolvidos novamente mesmo com `Prepared=true`.
Quando uma criação já foi iniciada, o contato persistido é mantido e o fluxo
continua buscando o pedido criado; não repete POST nem muda o contato no meio
de uma criação de resultado incerto.

Sem migration e sem alteração de Bling, Nuvemshop, gateways ou regras de estoque.
O cache genérico de contatos não é modificado: esta resolução consulta a conta
Tiny da operação e confere o documento a cada checkout que exige substituição.

Log de observabilidade:
`tiny paid checkout existing customer resolved`, com `cart_id`, `operation_id`,
`previous_contact_id` e `contact_id`, sem CPF, nome ou credenciais.

## Validação

- Regressão reproduziu o HTTP 400 anterior tanto em operação nova quanto em
  checkpoint preparado; passou com a correção.
- Testes de CPF/CNPJ com e sem máscara, dois contatos, lista incompleta,
  dados ausentes, contato alterado após consulta, inativo/excluído e HTTP
  403/429/503. Falha na consulta ou no checkpoint precede alterações de
  contato, criação/cancelamento e estornos financeiros/de estoque.
- Retomada após perda da resposta de criação e falha de vínculo: mesmo contato
  persistido, uma criação, um cancelamento e um estorno/relançamento de estoque.
- Suíte completa dos providers ERP com race detector passou.
- Testes Tiny do serviço de integração com PostgreSQL descartável e race
  detector passaram, usando o repositório e journal reais.
- `go build ./apps/api/...` passou.
- Convenções passaram com exclusão de `TestNoNewRawHttpxThrows`, cuja falha
  preexistente na integração Instagram foi documentada na investigação de estoque.
- E2E real na conta Tiny **ADABYTE LTDA** e banco descartável: origem
  `372346774` (nº 124), destino `372347011`. A reserva usou o contato de teste
  `897849766`, sem documento; o checkout reutilizou `897849768`, já cadastrado
  com CPF sintético. O pedido final foi aprovado com frete, pagamentos mistos,
  contas e estoque lançados; repetição criou zero pedidos adicionais. O contato
  da reserva permaneceu sem documento. Teste passou em aproximadamente 199 s.
- Ao terminar, os pedidos de teste ficaram cancelados, os lançamentos foram
  desfeitos e o CPF sintético foi removido do contato de teste. Nenhuma cobrança
  de gateway ou emissão fiscal foi realizada.

O cenário E2E usa `TINY_E2E_EXISTING_CONTACT=1`,
`TINY_E2E_CONTACT_DOCUMENT` com CPF sintético, `TINY_E2E_LAUNCHED_STOCK=1` e
`TINY_E2E_MIXED_PAYMENTS=1`, além de token/fixture da conta demo e banco isolado.
Ele recusa contato fora da fixture, documento já utilizado ou conta diferente.

## Publicação

Branch `fix/tiny-paid-checkout-contact`, baseada na `origin/main` `62f0493`,
sem incorporar commits alheios de staging. Este relatório não implica deploy.
Após publicar, uma nova tentativa poderá resolver o contato e retomar o mesmo
journal, sujeita às conferências atuais de nota fiscal, contas, estoque e itens.
A substituição comercial existente pode mudar o ID/número do pedido Tiny;
o pedido LiveCart permanece vinculado ao mesmo carrinho.

## Documentação oficial consultada

- [Listar contatos](https://api-docs.erp.olist.com/api-reference/contatos/listar-contatos)
- [Obter contato](https://api-docs.erp.olist.com/api-reference/contatos/obter-contato)
