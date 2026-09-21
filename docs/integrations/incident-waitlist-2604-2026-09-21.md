# Incidente Canto da Art — fila do produto 2604 — 21/09/2026

Este documento preserva a investigação inicial, antes da implementação. O estado final da correção está no [plano de implantação](waitlist-implementation-plan-2026-09-21.md); a [lista de recuperação](waitlist-manual-recovery-2026-09-21.md) contém a reconsulta mais recente e inclui uma ocorrência adicional identificada à tarde.

Investigação de produção somente leitura: PostgreSQL em transação READ ONLY, `default_transaction_read_only=on`, timeouts; logs pela CLI Railway. Nenhum GET do checkout público (ele muta a fila), replay, refresh OAuth ou chamada de escrita ao ERP. Nenhuma alteração funcional, commit ou publicação.

## Conclusão

A reposição foi reconhecida, os seis compradores documentados foram promovidos e as seis grades Tiny foram atualizadas com sucesso. Cerca de uma hora depois, a varredura de expiração de promoções removeu os itens e tornou as entradas `expired`. A ausência posterior na fila decorre do filtro de entradas ativas (`waiting`/`notified`), não de DELETE do histórico.

Há divergência de regras: o evento concede 7.200 minutos (cinco dias) aos carrinhos e há dois VIPs, mas a fila promovida expira independentemente após 30 minutos, inclusive antes do prazo do carrinho. A documentação interna e o texto do evento apresentam esse TTL como tempo adicional. A varredura não verifica prazo do carrinho, VIP nem pagamento. Ela roda globalmente dentro de GET de checkout.

A concorrência agrava o incidente: várias leituras selecionaram as mesmas entradas e repetiram a devolução ao estoque. Os logs demonstram 17 tentativas de decrementar itens já removidos e saldos locais temporariamente acima da Tiny. A deduplicação dos eventos de saída não protege as mutações anteriores.

## Identificação e versão

- Loja: Canto da Art (`a5403331-afd4-40ff-9bbe-4aa6f5322aee`).
- Produto: **2604 — PENDENTE SINO DE METAL - ALT 50 CM**, SKU `CA0350`, GTIN `7892458757291`.
- ID local `ada81faa-1b82-4625-93f9-0f3dc3056093`; Tiny `848594468`.
- Evento: Semana 15.09 ate 20.09 (`7cde8ecc-2f88-4de1-879c-518b2283cbeb`).
- Deployment Railway `447c6d09-d694-4b34-a6f6-bf16a35e2d28`, SUCCESS, criado 19/09/2026 21:06 UTC, commit `272eb5deff0a134d1561b0152366b69c3caec0f7`.
- Código inspecionado `ba4363d3df0f6ed0fccd4e1c77f63532b643c5bd`, árvore idêntica ao main implantado (fetch/recomparação em 21/09).

## Linha do tempo — Brasília (UTC−3)

1. **20/09 20:13:54**: @deirdre_iky recebe a unidade disponível normalmente, pedido1565. Não entrou na fila.
2. **20/09 20:14:01–20:15:10**: outros seis comentários geram seis entradas de espera, uma unidade cada. Todos têm `waitlist_joined` com status `sent`.
3. **20/09 23:59**: evento encerra. Carrinhos comuns seguem com janela até26/09; dois VIPs não expiram.
4. **21/09 06:59:32**: webhook de estoque persistido; log de reconciliação mostra `previous_stock=0`, `erp_available=9`, `admissible=9`. Portanto, há comprovação de nove unidades disponíveis, mas somente seis compradores na fila desse produto.
5. **06:59:39–07:02:24**: seis `waitlist.notified` e seis `stock.reserved(op=waitlist_promote)`. Logs mostram promoção e gravação de grade no ERP bem-sucedidas. Depois da promoção, saldo disponível3.
6. **07:29:32–07:31:54**: vencem os prazos individuais de30min registrados nos logs. Nenhum aviso de disponibilidade é enviado: o colaborador `NotifyWaitlistPromoted` é vazio, e o consumidor de `waitlist.notified` apenas observa o evento.
7. **07:57:22–07:59:47**: primeiras expirações de todos os seis. As grades Tiny são regravadas com um item a menos. Sweeps concorrentes continuam até08:00:27, sobrescrevendo o horário da mesma entrada e repetindo liberação local.
8. **07:57:59, 07:58:58 e 08:00:22**: sincronizações corrigem, respectivamente, local9→ERP5, local12→ERP7 e local15→ERP9. É divergência transitória real registrada; não há prova neste levantamento de uma nova venda consumindo essas unidades fantasmas.
9. **09:20:20**: logs `cart updated from the ERP order` e `cart followed the merchant's edit in the ERP` para Jeane, uma alteração. Atualmente a unidade voltou ao carrinho1586, com campos compatíveis com inserção pelo reflexo ERP. O log não permite identificar quem fez a edição externa.
10. **14:11:07**: Bete registra pagamento depois da remoção do item. Qualquer recuperação deve preservar esse pagamento e tratar a unidade ausente separadamente.
11. **Leitura de confirmação nesta investigação**: estoque local8; cinco compradores sem o item; Jeane com uma unidade; todas as seis entradas continuam `expired`.

## Compradores comprovados

| Pedido LiveCart | Comprador | External ID Tiny | Promoção | Primeira expiração | VIP | Situação consultada |
|---|---|---|---|---|---|---|
| 1564 | @goreti_medeiros16 | 848933209 | 21/09 06:59:39 | 21/09 07:58:08 | Sim | sem item |
| 1517 | @bete_abdon | 848934269 | 21/09 06:59:58 | 21/09 07:57:39 | Não | sem item; pagamento às 14:11 após remoção |
| 1586 | @jeanemarcia8 | 848934985 | 21/09 07:00:35 | 21/09 07:57:22 | Não | 1 unidade recolocada por reflexo ERP às 09:20 |
| 1592 | @marciapiantino | 848937397 | 21/09 07:01:18 | 21/09 07:59:47 | Não | sem item |
| 1545 | @neidemichelluzzi | 848787256 | 21/09 07:01:34 | 21/09 07:59:32 | Não | sem item |
| 1458 | @telascola | 848520067 | 21/09 07:02:24 | 21/09 07:58:35 | Sim | sem item |

Não há nove entradas persistidas nem nove comentários para esse produto: foram sete comentários compradores no total, um atendido diretamente e seis em espera. Não é possível atribuir a estimativa inicial de nove pessoas a registros que não existem no recorte.

## Defeitos e evidências de código

1. **Prazo da reserva promovida diverge do carrinho/VIP**. `apps/api/db/queries/waitlist.sql:107` seleciona apenas status da fila, prazo da fila e evento encerrado, sem consultar carrinho. `internal/inventory/service.go:394` calcula30min; a extensão do carrinho não reescreve esse prazo. A regra de encerramento da fila não atendida, em `integration/service.go:5566`, usa a carência do evento; essa outra tarefa não explica o ocorrido.
2. **GET público faz mutação global e lenta**. `internal/checkout/service.go:300` roda `ExpireNotifiedWaitlistSweep` durante leitura. Logs de cinco GETs nessa janela mostram ~141–176segundos. Não chamar esse endpoint em auditoria somente leitura.
3. **Expiração sem exclusão mútua/idempotência da mutação**. `inventory/service.go:695` decrementa item; erro apenas gera warning. `:709` chama ajuste e libera estoque mesmo assim. `:731` muda status só depois dos efeitos; SQL não exige status anterior. `erp/stock_service.go:189` incrementa estoque a cada chamada. Prova local e sinais reais convergem.
4. **Histórico de promoção é apagado**. `UpdateWaitlistItemStatus` (`waitlist.sql:22`) recebe `notified_at=NULL`, substitui `expires_at` por agora. Por isso a consulta isolada da linha parecia dizer que nunca houve promoção. O outbox e os logs preservaram a evidência.
5. **Nome do estado não significa aviso enviado**. `integration/service.go:5956` implementa `NotifyWaitlistPromoted` como no-op deliberado. Não há registro de aviso de promoção nos seis casos. Alguns textos de checkout ainda prometem aviso pelo Instagram; devem ser alinhados ao comportamento efetivo, sem inventar permissões de envio.
6. **Resposta de checkout pode ficar incoerente**. `checkout/service.go:280` monta os itens antes do sweep e `:319` lista a fila depois. A resposta que executa a remoção pode ainda mostrar o item; a leitura seguinte já não mostra. O histórico também depende de `notifiedAt`, apagado pela expiração.
7. **Risco adicional de pagamento**. A seleção também admite um carrinho já pago enquanto o reator ainda não converteu `notified` em `fulfilled`; reproduzido localmente. Não foi a causa da remoção de Bete: seu pagamento ocorreu horas depois.

## Reprodução local

Go overlay sem alteração dos arquivos funcionais, banco PostgreSQL descartável na porta5546, removido após o teste. Código real de seleção, serviço de expiração, decremento do item e liberação local. Sem chamada a ERP externo.

- Carrinho com cinco dias restantes: item removido e fila expirada.
- VIP `never_expires`: item removido e fila expirada.
- Pagamento já registrado, fila ainda `notified`: item removido (janela de risco).
- Controle com evento aberto: item preservado, nenhuma expiração.
- Duas varreduras com seleção concorrente da mesma entrada: `processed=2`, uma unidade removida, **duas** unidades devolvidas ao estoque.

Os testes PASS confirmam a reprodução do comportamento defeituoso, não uma correção. Fontes: `service_waitlist_2604_test.go`, `repro-overlay.json`, `repro-test.log` no diretório privado de evidências.

## Correção recomendada e recuperação

- Unificar validade da unidade promovida com a regra prometida do carrinho; respeitar VIP, pagamento e maior prazo já concedido. Se a regra desejada for outra, expor o prazo efetivo e acordá-la antes de alterar negócio.
- Retirar varredura global de GET; processar expiração em tarefa própria, por entrada, com claim/rechecagem sob transação e proteção contra concorrência de pagamento.
- Alteração local de fila+item+estoque atômica e condicional: exatamente uma liberação por unidade efetivamente removida. Propagar ERP por operação durável, recuperável, sem ignorar falhas.
- Preservar timestamps de promoção e prazo original; gravar motivo/horário de expiração e resultado da projeção ERP separadamente.
- Ajustar promessa de notificação na interface ao canal que de fato pode ser entregue.
- Recuperação de produção só após autorização específica e rechecagem de disponibilidade, grade e pagamento: cinco ausências; **não duplicar Jeane**; **não alterar retroativamente o pagamento de Bete**. Não basta recolocar `waiting` sem corrigir a regra, pois o mesmo ciclo se repetirá.

## Alcance e limites

A varredura é compartilhada e pode afetar outras lojas/produtos. A consulta adicional encontrou seis entradas expiradas hoje em outros produtos da Canto da Art (2427,2514,1522,2569,2594,2531), incluindo um VIP. Isso é triagem; cada uma requer conferência própria antes de classificá-la como indevida ou reparar.

Não foram consultados endpoints Tiny ao vivo nesta investigação: a execução e as grades foram comprovadas pelos logs de produção e pelo banco. Não há indício de perda do webhook desta reposição; ele está no outbox e o saldo foi aplicado antes das seis promoções.

Evidências privadas em `/home/alisson/Desktop/livecart/.tiny-demo/incident-waitlist-2604/`: consultas01–05, resultados JSONL, deployments, logsRailway, reprodução local e análiseUI. Não versionar os arquivos brutos por conterem dados operacionais/compradores e URLs de checkout.
