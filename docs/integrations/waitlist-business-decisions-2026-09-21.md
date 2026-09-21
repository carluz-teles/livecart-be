# Revisão das regras de fila e prazo — 21/09/2026

Estado: **14 decisões confirmadas e implementadas na branch `fix/waitlist-lifecycle`**. Detalhes de implantação e validação: `waitlist-implementation-plan-2026-09-21.md`. Produção somente foi consultada; reinclusões do incidente serão manuais.

## Regras expressamente confirmadas pelo usuário

- Quando houver estoque, a unidade deve entrar definitivamente no carrinho de quem aguarda.
- A unidade promovida não terá expiração individual nem será removida por um timer próprio da fila.
- Confirmado na primeira resposta: o carrinho inteiro continua expirando normalmente quando seu prazo acaba sem pagamento; os itens promovidos acompanham essa expiração geral.
- Carrinhos com produtos ainda aguardando terão prazo normal X mais prazo extra configurável Y.
- Confirmado na segunda resposta: X+Y conta do encerramento do evento. Exemplo: X = 5 dias + Y = 1 dia = vencimento seis dias após encerramento.
- Confirmado na terceira resposta: quando todos os pendentes forem atendidos, manter a data X+Y já concedida, sem reiniciar ou encurtar.
- Confirmado na quarta resposta: ao vencer X+Y de um carrinho comum, encerrar o carrinho e a espera ainda não atendida. Uma reposição posterior exige nova compra e não reabre automaticamente o carrinho expirado.
- Confirmado na quinta resposta: para VIP, a espera também não expira. Preservá-la até atendimento ou cancelamento explícito; o encerramento do evento não a remove automaticamente.
- Confirmado na sexta resposta: quando o cliente paga os produtos disponíveis, encerrar a espera restante. Os produtos pendentes exigem um novo pedido do cliente; não adicionar automaticamente ao pedido pago nem criar outra compra automaticamente.
- Confirmado na sétima resposta: conceder Y apenas a quem ainda tiver produto aguardando estoque no momento do encerramento do evento. Se todos os produtos já foram atendidos antes desse momento, aplica-se somente X, respeitada a exceção de não expiração do VIP.
- Confirmado na oitava resposta: atender parcialmente conforme o estoque disponível. Se o cliente pediu três unidades e chegou uma, adicionar uma ao carrinho e manter as duas restantes na mesma posição da fila, sem reiniciar prazo ou perder prioridade.
- Confirmado na nona resposta: a reposição atende por ordem global de entrada na fila por loja e produto, independentemente do evento e sem prioridade automática para VIP. Lojas distintas permanecem isoladas.
- Confirmado na décima resposta: desistir de todos os produtos ainda em espera não retira o adicional já concedido. Manter a data X+Y apresentada para os itens disponíveis no carrinho.
- Confirmado na décima primeira resposta: alterações de X/Y após o encerramento aplicam aumentos aos carrinhos ainda abertos; reduções preservam os prazos já concedidos. Alterar configuração não autoriza reabrir carrinhos expirados ou cancelados. Y continua restrito aos carrinhos elegíveis ao adicional no encerramento.
- Confirmado na décima segunda resposta: Y aceita de 0 a 30 dias, configurável em minutos, horas ou dias. Zero desativa somente o prazo adicional; não elimina as demais regras da fila ou o prazo normal X.
- Confirmado na décima terceira resposta: preservar o preço das unidades no momento em que o cliente entrou na fila. Alterações posteriores no catálogo não reprecificam essas unidades quando o estoque chega.
- Confirmado na décima quarta resposta: quantidades adicionais solicitadas depois entram no fim da fila, com a data da nova solicitação. As quantidades anteriores preservam a posição original. Não somar novas unidades à prioridade antiga.
- O usuário fará a inclusão manual dos itens ausentes; a investigação fornece compradores, produtos e quantidades.

## Primeira rodada — decisões concluídas

1. **Confirmado:** depois da promoção, o carrinho inteiro continua sujeito à expiração normal. O produto não tem expiração separada.
2. **Confirmado:** X+Y conta do encerramento do evento; Y não começa com a reposição.
3. **Confirmado:** manter a data X+Y já concedida após atender os pendentes, sem reiniciar ou encurtar.

Todas as alternativas registradas abaixo foram escolhidas expressamente pelo usuário. Alterações em produção permanecem fora desta etapa; o usuário fará as reinclusões manuais.

## Segunda rodada — decisões concluídas

4. **Confirmado:** ao vencer X+Y sem estoque em carrinho comum, encerrar o carrinho e a espera. Uma reposição posterior exige nova compra.
5. **Confirmado:** VIP mantém a espera sem prazo até receber o produto ou alguém cancelar.
6. **Confirmado:** pagar os itens disponíveis encerra a espera restante; os produtos pendentes exigem novo pedido do cliente.

## Terceira rodada — decisões concluídas

7. **Confirmado:** apenas quem ainda aguarda estoque no encerramento do evento recebe Y. Ter passado pela fila antes, com todos os produtos já atendidos, não concede o adicional.
8. **Confirmado:** se pediu três unidades e chegou uma, adicionar uma ao carrinho e manter as duas restantes na mesma posição da fila.
9. **Confirmado:** ordem global de entrada por loja e produto, independentemente do evento ou de ser VIP.

## Quarta rodada — decisões concluídas

10. **Confirmado:** desistir da última espera preserva o prazo X+Y já concedido, sem encurtar a data mostrada.
11. **Confirmado:** aplicar aumentos de X/Y a carrinhos ainda abertos, preservando os prazos já concedidos quando houver redução. Não antecipar vencimentos existentes.
12. **Confirmado:** Y de 0 a 30 dias, em minutos, horas ou dias; 0 desativa o adicional.

## Quinta rodada — decisões concluídas

13. **Confirmado:** preservar o preço de quando o cliente entrou na fila para as unidades já pedidas.
14. **Confirmado:** preservar a prioridade da primeira unidade e enfileirar as duas novas pela data da nova solicitação, ao final da fila.

## Encaminhamento técnico

As perguntas de regra desta revisão estão concluídas. Concorrência, idempotência, transações, representação dos preços por solicitação, migração de dados e atualização da interface são decisões de implementação que devem cumprir o contrato acima. Não reinterpretar recomendações do documento auxiliar como novas escolhas do usuário.

Manter isolamento entre lojas, estoque disponível do ERP como referência e os pagamentos já registrados. A implantação da mudança não deve reabrir automaticamente as entradas expiradas do incidente; o usuário está fazendo a recuperação manual.

## Inconsistências confirmadas na versão anterior

- `FinalizeCartsByEvent` já soma X+Y a carrinhos elegíveis.
- A promoção também cria prazo individual `now+Y` e estende o carrinho por GREATEST.
- GET do checkout roda varredura global que remove a unidade quando vence o prazo individual.
- O fechamento da fila ainda aguardando é agendado em X, sem Y.
- A varredura não protege VIP/pagamento nem serializa efeitos: duas execuções devolvem a mesma unidade duas vezes.
- A edição de X propaga deltas; edição de Y apenas salva a configuração e a tarefa de fechamento da fila não acompanha de forma equivalente.

Separar os conceitos: prazo do carrinho, existência de unidades aguardando e histórico de unidades já promovidas. Não reutilizar o estado de notificação como autorização para retirar mercadoria.

Lista de recuperação manual: `waitlist-manual-recovery-2026-09-21.md`.
