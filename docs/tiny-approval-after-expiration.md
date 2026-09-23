# Aprovação Tiny após expiração

Regra confirmada pelo lojista em 23/09/2026: uma aprovação na Tiny representa o reconhecimento do pagamento externo, mesmo quando o carrinho expirou no LiveCart. A ausência de gateway na Maricelia Decor é intencional.

## Recuperação da venda

Ao receber uma aprovação já identificada como pertencente à loja e ao carrinho, o backend lê novamente a venda na Tiny. O status atual, os itens, o frete e o total vêm da mesma resposta de `GET /pedidos/{id}`. Um pedido que voltou a rascunho ou foi cancelado não é recuperado por uma notificação antiga.

A transação local:

1. Bloqueia a compra, valida a integração ativa e impede conflito com pagamento existente, estorno, revisão financeira ou edição pendente.
2. Recupera o carrinho diretamente como `checkout` + `paid`, sem reabrir uma oferta pagável. Um carrinho novo do mesmo comprador permanece independente.
3. Reflete a grade atual da Tiny, preservando a origem dos itens em compras agrupadas.
4. Registra um pagamento `erp_manual`, identificado por `erp-{id}`, com total verificado na Tiny. A data é a do reconhecimento no LiveCart, não uma data bancária presumida.
5. Registra a expiração anterior, o estado anterior do ERP, os itens e preços anteriores, a venda externa e os valores conferidos. O evento `payment_confirmed` conserva esses dados no histórico do pedido.
6. Publica `cart.paid` e a releitura de estoque pela outbox na mesma transação. Uma falha desfaz a recuperação e permite retentativa.

O saldo local dos produtos envolvidos fica conservador até uma leitura atual do estoque **disponível**. Não se descontam novamente as unidades já contabilizadas pela Tiny. As esperas encerradas na expiração não são reativadas; somente a composição verificada da venda é recuperada.

O reator de pagamento já reconhece a origem ERP: conclui a finalização local e não cria outra venda, cobrança ou lançamento de estoque na Tiny. Uma tarefa antiga de expiração não pode cancelar a compra recuperada. O frontend identifica a recuperação no aviso e no histórico.

Pedidos manualmente cancelados, reembolsados, com pagamento anterior conflitante, com produtos sem vínculo Tiny ou com edição pendente continuam protegidos. O faturamento isolado não passa a ser prova de pagamento: deve existir uma aprovação recebida pelo fluxo de aprovação.

## Bloqueios de estoque

O incidente de produção de 23/09 às 10h53 BRT mostrou a recuperação de estoque bloqueando `integrations` antes do checkpoint, enquanto o webhook bloqueava o produto/checkpoint antes de `integrations`. A gravação do indicador de webhook foi movida para o começo da mesma transação. A ordem passa a ser integração → produto → checkpoint; recebimento, invalidação e outbox continuam atômicos.

O teste `TestTinyWebhookUsesRecoveryLockOrder` reproduziu a ordem incompatível antes do ajuste e passou depois. A regra segue a [orientação do PostgreSQL para prevenção de deadlocks](https://www.postgresql.org/docs/current/explicit-locking.html#LOCKING-DEADLOCKS).

## Publicação e acompanhamento

Não há migration nem backfill nesta alteração. O código deve passar pelo fluxo de staging e PR para produção. Nenhum pedido de produção foi alterado durante a investigação.

As aprovações históricas podem ser retomadas por nova entrega ou pela varredura existente. Essa varredura consulta estados parados há 24 horas e tem limite por rodada; publicar não significa correção imediata de todo o histórico. Não chamar a recuperação em massa sem conferir os vínculos e as proteções financeiras.

Log de sucesso: `expired cart paid following verified Tiny approval`. Conferir pagamento único, mesmo `external_order_id`, finalização `done`, histórico `tiny_approved_after_expiry` e conclusão da releitura de estoque.

Validação: suíte completa com PostgreSQL e Redis locais; testes com detector de corrida para concorrência, rollback, duplicação e ordem dos bloqueios; builds de backend e frontend. A API da Tiny foi simulada nos testes da correção. A conta real não recebeu escritas.

Referência do contrato: [obter pedido na API Olist/Tiny](https://api-docs.erp.olist.com/api-reference/pedidos/obter-pedido).
