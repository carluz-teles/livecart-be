# Tiny: aprovação externa e busca de importação

Investigação de 17/09/2026. A regra confirmada pelo lojista é que colocar a venda
na situação **Aprovado** significa pagamento recebido fora do checkout.
Faturamento, emissão de nota e lançamento de contas isoladamente não são prova
desse gesto. As consultas de produção desta investigação foram somente leitura.

## Defeitos encontrados

- A aprovação atualizava `carts` e `cart_payments`, mas não emitia `cart.paid`.
  Sem esse evento, não havia criação do agregado `orders` usado pelo painel nem
  execução dos consumidores do pagamento. Foram identificados inicialmente 13 casos de hoje
  nessa condição; a última leitura encontrou 15, com a operação ainda ativa.
  Sete não tinham itens em aberto e oito já tinham saldo adicional, portanto
  materializar todos usando a grade atual sem conferir seria incorreto.
- A gravação da situação era deduplicada antes de tentar refletir o pagamento.
  Uma falha após gravar `aprovado` impedia a recuperação com a mesma notificação.
- A notificação de aprovação era processada em goroutine. Reinício ou falha
  podia perder o trabalho; uma situação posterior ocultava a aprovação no sweep.
- Falhar ao ler o total do ERP resultava em pagamento de valor zero.
- A busca de importação enriquecia até 20 resultados com detalhes e estoque.
  Na Tiny cada detalhe exige também consultar o disponível: até 42–43 chamadas
  antes da resposta, contra um timeout do navegador de 10 segundos.
- Após falha de renovação OAuth, a integração ficava em `error`, estado que a
  busca de credenciais do callback não aceitava.

## Comportamento corrigido

1. A observação de aprovação é persistida no outbox antes de responder ao
   webhook. O consumidor verifica novamente a loja e a integração. Pedidos
   alheios são ignorados, e falhas transitórias usam a fila existente.
2. A projeção do pagamento pode repetir independentemente do histórico de
   situações. O total precisa ser lido com sucesso e ser positivo.
3. Pagamento, cobertura dos itens e `cart.paid` são gravados na mesma transação.
   Reentrega não duplica o livro de pagamentos nem o evento. Edições de grade
   ainda pendentes e pagamentos em revisão continuam protegidos.
4. O pagamento originado no ERP materializa o pedido local. Os consumidores
   reconhecem `erp_manual` e não fazem checkout, lançamentos ou reescritas na
   venda que originou a confirmação. A finalização local exige o mesmo vínculo
   de pedido externo do snapshot.
5. `GET .../products?summary=true` retorna prévias identificadas por
   `detailsPending`. Apenas o produto escolhido usa o novo
   `GET .../products/{productId}`, que consulta detalhes e estoque disponível.
   A busca antiga continua compatível quando `summary` não é solicitado.
6. A busca limita resultados a 20, tem prazo de 25 segundos (45 para detalhes), distingue
   indisponibilidade de produto inexistente e registra duração/quantidade no
   log `ERP product search completed`. O frontend cancela consultas obsoletas,
   reutiliza listagens por 30 segundos e não refaz consultas ao trocar de aba.
7. Reconexão Tiny aceita a integração em erro, preservando a mesma linha e
   recusando credenciais de outro ERP.

## Validação

- Testes com PostgreSQL descartável dos domínios integração, ERP, providers,
  pedidos e pagamentos; testes de concorrência selecionados e build backend.
- 35 testes do frontend e build Next.js.
- Conta Tiny de testes autenticada, identidade ADABYTE conferida antes da escrita.
  Venda de teste `372818194`: aprovação real, webhook pelo túnel, pagamento
  R$49,90, pedido local pago e finalização concluída. Duas reentregas preservaram
  exatamente um pagamento e um evento `cart.paid`.
- Busca real `LC-TINY`: duas prévias em 243 ms com dois GETs. Produto escolhido:
  dois GETs em 493 ms, consultando disponível. Estas medições iniciais não incluíam o limitador de consultas. Com ele
  ativo, a busca por nome levou 5,027 s, detalhes/estoque 5,028 s e a nova busca
  prioritária por GTIN 2,471 s (um GET). Medições da conta demo, não SLA
  nem benchmark da produção. O teste com 20 resultados comprova que o modo de
  prévias não consulta detalhes de nenhum dos 20 produtos.

## Publicação e histórico

Os fluxos compartilhados de busca e pagamentos externos também são usados por
outros ERPs; os testes de Bling permanecem verdes. O frontend é compatível com
respostas antigas, e o backend mantém o modo anterior sem `summary`.

Esta alteração não inclui migration nem reparação automática de pagamentos
antigos. Existem correções anteriores nesta branch, com migration 160; consultar
`erp-resync-and-closed-carts.md` ao revisar o conjunto para produção.

Os registros históricos com carrinho pago e pedido ausente precisam de reparo
separado, autorizado e conferido contra o livro já existente; nunca registrar
outro pagamento para materializar o pedido. Pedidos faturados sem observação de
aprovação e com divergência de grade exigem conciliação específica. O deploy
não deve converter faturamento em confirmação financeira por inferência.

A investigação específica de código de barras, espera por cota e limites de
tempo está em `tiny-barcode-search-timeouts.md`.
