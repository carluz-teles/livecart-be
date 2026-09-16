# Tiny: contatos legados e endereço na retomada do checkout

## Evidências

Investigação de produção somente leitura, por Railway CLI, PostgreSQL em
transação `READ ONLY` e GETs da API Tiny. A versão em produção era `ff9195e`
(PR #80), deployment `51b12f79-18cc-4982-9ef4-9f49efa30ef3`, publicado em
15/09/2026 às 19:33, America/Sao_Paulo. Portanto as novas tentativas já usavam
a correção anterior de resolução de contato; não era um deploy ausente.

### Pedido 1529

- Carrinho `fa621d08-e5ea-4736-8ed8-a8f5ca841ef3`.
- Tiny nº 27849, ID `848756264`.
- Última tentativa observada: 16/09/2026 às 11:18:20, America/Sao_Paulo.
- Falha: contato do comprador inativo ou excluído.

A busca pelo documento retornava quatro cadastros: dois ativos e dois
excluídos. O código rejeitava toda a lista quando encontrava um excluído.
Além disso, rejeitava múltiplos IDs ativos sem comparar os demais dados.
Os dois cadastros ativos conferem com CPF, nome (normalizando acentos),
email e telefone do checkout. O contato da reserva não tem documento.
O journal estava preparado, mas ainda sem criação iniciada ou pedido destino.

### Pedido 1516

- Carrinho `25807ae2-2b7c-43fa-9fbc-68e75c3a166f`.
- Origem Tiny nº 27830, ID `848742294`.
- Destino Tiny nº 27845, ID `848748282`, já criado pela tentativa anterior.
- Última tentativa observada: 16/09/2026 às 11:18:32, America/Sao_Paulo.
- Falha: endereço de entrega divergente no pedido substituto.

A correção anterior resolveu o contato correto e criou o destino com frete
R$ 13,50 e total R$ 196,76. A Tiny devolve `enderecoEntrega: null`, usando
`cliente.endereco`; os sete campos desse endereço conferem com o checkout.
O comparador da substituição exigia o objeto separado. A conciliação somente
leitura já reconhecia essa representação, mas o caminho de finalização não.
O journal preservou a origem: sem cancelamento ou estorno de estoque iniciado.

Esses dois cenários não eram cobertos pelos testes anteriores: a resolução
assumia documento único e a conferência da substituição assumia endereço de
entrega separado. Foram adicionadas regressões para ambos.

## Correção

### Contatos

- Ignorar cadastros inativos/excluídos na seleção; eles não invalidam um ativo.
- Preservar o vínculo atual se estiver ativo e com o mesmo documento.
- Havendo vários ativos e nenhum vínculo confirmado, exigir nome, email e
  telefone correspondentes, além do documento. Entre os correspondentes,
  selecionar o menor ID para que a ordenação da busca não altere o resultado.
- Reler e confirmar o cadastro selecionado antes de persistir o vínculo.
- Na atualização do contato, omitir o CPF/CNPJ somente quando uma releitura
  confirmar que já é o mesmo; assim não reenviar um documento legado
  duplicado desnecessariamente. Documento diferente continua bloqueado.
- Não excluir, fundir ou reativar contatos. Cadastros ambíguos e respostas
  incompletas continuam impedindo a alteração automática.

### Entrega e retomada

O comparador compartilhado usa `cliente.endereco` apenas se
`enderecoEntrega` estiver ausente/nulo. Um endereço separado explícito,
mesmo parcial, continua sendo obrigatório na comparação. Número,
complemento, bairro, cidade, estado e CEP continuam sendo conferidos.

O pedido 1516 retoma o destino já salvo `848748282`; não deve criar outro.
As proteções existentes de itens, frete, pagamento, contas, nota fiscal e
estoque continuam aplicadas antes de cancelar a origem e concluir o vínculo.

Conflitos verificáveis de contato ou do destino passam a usar
`TinyCheckoutReconciliationError`, já convertido pelo endpoint de nova
tentativa em HTTP 422 (`ERP_RETRY_INVALID_STATE`) com motivo específico.
Falhas técnicas de transporte/servidor não são convertidas em sucesso.

Logs adicionais, sem documento, email, telefone ou endereço:

- `tiny checkout contact candidates resolved`: ID selecionado, quantidade
  de candidatos e de ativos, confirmação dos demais dados do comprador.
- `tiny checkout delivery address verified from customer`: carrinho,
  operação e ID Tiny do destino.

Mudanças restritas à Tiny, sem migration, alteração de gateway ou frontend.

## Validação

- Regressões dos dois defeitos falharam antes da correção e passaram depois.
- Suíte completa de providers ERP com race detector passou.
- Testes Tiny e de resposta HTTP de nova tentativa com PostgreSQL descartável
  e race detector passaram; `go build ./apps/api/...` passou.
- Convenções passaram excluindo `TestNoNewRawHttpxThrows`, falha preexistente
  da integração Instagram já registrada no incidente de estoque.
- Reprodução local, sem rede/escritas, usando cópias privadas das respostas
  reais: pedido 1516 passou na conferência comercial, grade e envio; pedido
  1529 selecionou um cadastro ativo com todos os dados correspondentes.
  As respostas privadas não foram adicionadas ao repositório.
- Regressão de retomada com destino já criado e estoque lançado: reaproveita
  o destino, estorna/relança uma vez; repetir a conclusão não repete essas
  operações. Destino explícito divergente retorna conciliação e preserva a
  origem sem estorno/cancelamento.
- E2E real na conta de testes **ADABYTE LTDA**, banco local descartável,
  contato de reserva separado e comprador previamente cadastrado:
  origem `372468724` (nº 126), destino `372469225`.
  A Tiny retornou endereço de entrega nulo e endereço do cliente correto;
  finalização aprovou frete, pagamentos mistos, contas e estoque. Repetição
  criou zero pedidos adicionais. O CPF existente foi preservado no PUT.
  O teste passou em aproximadamente 209 segundos.
- Limpeza E2E concluída: pedidos de teste cancelados, contas e estoque
  estornados; documento sintético removido e endereço de teste restaurado.
  Nenhuma cobrança real ou emissão fiscal foi realizada.

O E2E novo usa `TINY_E2E_CUSTOMER_DELIVERY=1`, junto de
`TINY_E2E_EXISTING_CONTACT=1`, `TINY_E2E_LAUNCHED_STOCK=1` e
`TINY_E2E_MIXED_PAYMENTS=1`. Exige conta demo e fixtures próprias existentes.
A seleção entre documentos legados duplicados foi validada com regressões
e respostas reais somente leitura; não foram criados duplicados na demo.

## Limites e publicação

A consulta ampliada encontrou também quatro erros contendo endereço nos
pedidos 1469, 1471, 1476 e 1495. São registros de 12 a 14/09, com divergências
adicionais e sem o mesmo checkpoint de substituição. Não são evidência de
quatro novas ocorrências deste defeito. Existem ainda dez pendências antigas
de outras categorias, incluindo estoque insuficiente, movimento incerto,
pedido excluído e despesas. Não foram reprocessadas nem declaradas resolvidas.

Branch isolada `fix/tiny-checkout-contact-delivery-validation`, baseada na
main `ff9195e`, para não transportar mudanças alheias de staging ao PR.
Publicação de código não significa recuperação dos pedidos: após o deploy,
uma nova tentativa precisa reler e validar a situação atual no ERP.
Nenhuma nova tentativa ou escrita foi executada em produção nesta investigação.

## Documentação consultada

- [Listar contatos](https://api-docs.erp.olist.com/api-reference/contatos/listar-contatos)
- [Obter contato](https://api-docs.erp.olist.com/api-reference/contatos/obter-contato)
- [Obter pedido](https://api-docs.erp.olist.com/api-reference/pedidos/obter-pedido)

As situações dos contatos e os campos foram conferidos na documentação.
A representação do endereço nulo foi confirmada pelos GETs de produção e
pelo teste real na demo; não se atribui essa garantia ao texto da documentação.
