# SYNC do catálogo e comentários em pedidos encerrados

## Problema corrigido

O SYNC usava `erp-resync:<integration_id>` como ID fixo de uma tarefa de até
45 minutos. Uma tarefa arquivada continuava ocupando esse ID. O endpoint
tratava o conflito como sucesso, gravava uma marca de execução e não iniciava
trabalho novo. A marca e os contadores também disputavam a atualização do
JSON de configurações da integração.

Separadamente, um carrinho VIP com pagamento local pendente podia continuar
recebendo comentários mesmo após ser faturado no ERP. O processamento tentava
editar o mesmo documento repetidamente. Reentregas também repetiam a reserva
de itens já confirmados e podiam ignorar o intervalo entre tentativas.

## Comportamento novo

- `erp_resync_jobs` guarda **uma linha por integração**, atualizada no lugar.
  Contém o resumo e uma lista temporária de IDs do catálogo, descartada ao
  concluir. Não armazena respostas dos provedores nem histórico de execuções.
- O início e o próximo lote são gravados junto do comando na outbox. Cada
  lote processa até cinco produtos em sequência, com até dois minutos de
  execução. O cursor é salvo após cada produto.
- Cliques concorrentes compartilham a execução ativa. Lotes antigos não
  sobrescrevem o avanço de outro trabalhador. A recuperação consulta leases
  vencidos, comandos sem avanço e tentativas adiadas a cada 30 segundos.
- Recuperar uma fila atrasada mantém válido o comando já enfileirado; uma
  entrega adicional recebe outro ID de tarefa e não depende do ID arquivado.
- Timeout, falha de rede, rate limit e disputa do espelho de estoque preservam
  o cursor e adiam a execução. A espera cresce até 15 minutos, respeitando um
  `RetryAfter` maior. Uma execução com interrupções persistentes por 72 horas
  termina como falha na próxima liberação do lote. Outros erros de produto
  ficam no contador de falhas e nos logs; não são anunciados como sucesso.
- A API mantém os campos antigos e acrescenta `erpResync`. O frontend mostra
  percentual, processados, atualizados, falhas, restantes e horário do avanço.
  O resumo permanece ao concluir. Uma marca antiga sem checkpoint aparece
  como execução anterior sem acompanhamento e permite iniciar um novo SYNC.
- Consultas e índices de unicidade usam a mesma regra de elegibilidade para
  carrinhos. Estados fechados para novos itens no ERP exigem uma nova compra,
  preservando itens e pagamentos da compra anterior. Isso se aplica também
  a compradores não VIP.
- Comentários bloqueados por documento encerrado permanecem pendentes, sem
  insistência automática enquanto a condição não mudar. Conciliação dos itens,
  reabertura ou término do carrinho libera a avaliação novamente. Itens já
  confirmados não provocam outra tentativa de alteração do ERP.

## Publicação

1. Publicar backend com migrations **158 e 159**, junto de seu código.
2. Publicar frontend com o contrato novo. A resposta conserva os campos de
   compatibilidade para permitir a publicação do backend primeiro.
3. Iniciar um novo SYNC pela tela. O deploy não reenvia o comando legado
   arquivado nem faz conciliação dos pedidos históricos.
4. Acompanhar `ERP resync queued`, `ERP resync progress`,
   `ERP resync recovery dispatched` e `ERP resync product failed`, filtrando
   `integration_id`/`run_id`. O log final de cobertura de SKU/código de barras
   continua disponível em `ERP resync identifier coverage`.
5. Comentários que exigem conferência registram `comment awaits ERP reconciliation`.
   Pendência de item não é automaticamente tratada como confirmação.

As migrations de subida não excluem pedidos, itens ou pagamentos. O rollback
da 159 só pode restaurar os índices anteriores se não houver uma nova compra
aberta ao lado de um pedido faturado ainda não pago localmente. Havendo
conflito, o rollback falha; não apagar ou juntar pedidos para forçá-lo.

## Validação local

- PostgreSQL isolado: suíte de integração dos pacotes `integration`, `live`,
  `events`, `order` e `cmd/http-server` com `-tags=integration`.
- Novos cenários: início concorrente, protocolo de conexão de produção,
  retomada em outra instância, entrega repetida, lease vencido, recuperação de
  fila atrasada, erro temporário, progresso final e preservação de metadata.
- Carrinhos: equivalência entre consulta e índice para VIP/não VIP, preservação
  da compra antiga, comentário confirmado e bloqueio até conciliação.
- Frontend: 33 testes da suíte de operações, build, verificação do painel em
  Chromium nas larguras 390 e 1440, estados em andamento/retentativa/concluído
  com falhas, valor acessível da barra e ausência de rolagem horizontal.
- Build do backend concluído. Nenhuma alteração de dados ou fila em produção
  foi necessária para estes testes.
