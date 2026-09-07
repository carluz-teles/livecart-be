# Publicação em staging e acompanhamento

Autorização de 07/09/2026: atualizar `stg`, fazer commit e enviar diretamente para
`origin/stg` nos repositórios backend e frontend. O usuário testa e abre o PR para
produção. Esta autorização não inclui merge, migrações ou reparos em produção.

## Há mudança destrutiva?

As migrações **UP 149–152 não apagam pedidos, produtos, pagamentos ou colunas
existentes**. Criam tabelas, campos e índices. A 149 substitui um CHECK e um índice
para aceitar o estado de reflexão do ERP; esses objetos são recriados na mesma
migração. Isso pode bloquear transações durante a alteração de tabelas/índices.

O **DOWN é destrutivo para os dados novos**: remove os registros de trabalho de
comentários, tentativas de pagamento e orçamentos de API, além de colunas. Não
executar rollback de schema para desfazer um deploy depois de gerar cobranças.

Não é um PR de risco zero. Ele altera caminhos centrais de captura, reserva,
confirmação de itens, pagamento, PIX e envio de mensagens. Os principais pontos
para testar antes de produção são:

| Ponto | O que pode acontecer | Verificação em staging |
|---|---|---|
| Schema | Backend novo sem migrações falha ao consultar os campos novos | Confirmar `migrations applied` e versão 152, sem estado dirty |
| Assinatura Instagram | Segredo ausente/incorreto gera 401 e interrompe captura; exigência agora é o padrão | Entrega assinada válida aceita; assinatura inválida recusada; não testar com comentário de compra real |
| Cota Tiny | Pedidos podem esperar mais; outros apps continuam disputando a mesma cota | Conferir reset, volume de 429 e progresso das recuperações |
| Pagamento alterado | Cobrança paga com carrinho diferente fica em conferência, bloqueando nova cobrança/finalização | Exercitar em sandbox o aviso no checkout e no painel, conferir ledger sem duplicação |
| PIX antigo | Pode ser necessário esperar a expiração antes de gerar outro | Alterar frete/cupom e verificar a mensagem; não executar refund como teste |
| Recuperação | Trabalhadores retomam comentários e itens novos elegíveis automaticamente | Ver sucessos e pendências nos logs e conferir os dados do pedido |

Em staging, as migrações rodam automaticamente quando o ambiente está configurado
como não produção. Em produção, o bootstrap só as aplica se
`RUN_MIGRATIONS_ON_STARTUP=true`; caso contrário, devem ser aplicadas pelo fluxo
controlado existente antes do backend novo. Não alterar variáveis ou schema de
produção como parte desta publicação em `stg`.

Backend com migrações primeiro; frontend compatível em seguida. O frontend novo
usa campos opcionais, mas as proteções de pagamento dependem do backend novo.

## Logs disponíveis em nível info/warn/error

Staging já usa JSON estruturado e inclui `env`. Filtrar primeiro pelo ambiente e
deployment corretos. `duration` e `reset_in` usam segundos no encoder de produção.

| Mensagem exata | O que permite acompanhar |
|---|---|
| `provider HTTP 429` | Uma resposta HTTP recusada: `integration_id`, `method`, `path`, `rate_limit`, `rate_remaining`, `rate_reset`, `retry_after` |
| `rate limited request (429): waiting for reset` | Retentativa aguardando, com método, tentativa e `reset_in` |
| `rate limited request: reset exceeds remaining deadline` | A espera não cabe no contexto; o 429 retorna ao fluxo responsável |
| `comment recovery completed` | Processamento pendente de comentário terminou sem erro, com `comment_id`, `account_id`, `media_id` |
| `comment remains pending` | Comentário segue pendente; inclui identificador e erro |
| `comment recovery batch` | Lote com `selected`, `attempted`, `succeeded`, `deferred`, `busy`, `invalid` e `duration` |
| `comment ERP synchronization remains pending` | A etapa ERP do comentário falhou; contém `cart_id` |
| `ERP item recovery acknowledged` | Rodada de recuperação e chamadas de confirmação terminaram sem erro: `cart_id`, `store_id`, `event_id`, `items` |
| `ERP cart remains pending` | Carrinho ainda aguarda ERP, identificado por loja e carrinho |
| `ERP item recovery batch` | `selected`, `attempted`, `acknowledged`, `deferred`, `duration` |
| `payment requires quote review` | Dinheiro foi registrado para conferência; `cart_id`, `store_id`, `payment_id`; é diferente de webhook duplicado |
| `payment already recorded or obsolete` | Evento repetido/antigo não foi aplicado como novo pagamento |
| `instagram webhook rejected: signature check failed` | Rejeição com `signature_outcome` e `enforcing` |
| `persisting API budget` | Falhou a gravação do feedback da API; investigar banco/limiter |

Não somar logs do HTTP e dos retries como se fossem novas requisições. Os resumos
são por lote e instância, emitidos quando existe trabalho selecionado; não são um
medidor contínuo de backlog. `succeeded` pode incluir um comentário já concluído
por outro caminho. Uma confirmação sem erro também não prova que não chegaram
novos itens concorrentes ao carrinho. A consulta em
[observability-recovery.sql](observability-recovery.sql) mostra pendências e sua
idade no estado atual do banco.

Nas primeiras horas, observar se os sucessos acompanham as tentativas e se a idade
das pendências diminui. Crescimento persistente, 401 de assinatura ou pagamento em
conferência exigem investigação. Tráfego parado não demonstra correção do fluxo.

## Escopo que não foi reparado por este commit

- Pedidos históricos dos clientes continuam sujeitos ao plano de reconciliação
  aprovado separadamente. As migrações não os reprocessam.
- A recuperação durável de webhooks **Tiny de produto/estoque** permanece uma
  lacuna distinta dos trabalhadores de comentários Instagram e itens de carrinho.
- A conta Tiny sem CNPJ nos metadados compartilha orçamento por loja, não entre
  várias lojas conectadas à mesma conta externa.
- Dois testes antigos de estoque seguem pulados: `TestDefeitoEspelhoAplicaLeituraAnteriorAoNossoMovimento` e `TestEspelhoDoERPNuncaInventaUnidade`. A suíte aprovada não significa que esses dois testes passaram.

O workflow de testes passa a rodar também em pushes para `stg`. Isso não configura
automaticamente uma barreira de testes no deploy Railway. A consulta de produção
mostrou os dois serviços ativos vindos de `main`; somente as branches `stg` são
enviadas nesta publicação. Os logs novos aparecem no ambiente que efetivamente
publicar esses commits, não na produção enquanto ela permanecer na versão anterior.

## Validação antes do commit

- Backend: suíte completa `go test -race -tags=integration -p=2 -count=1 -timeout=180s ./apps/api/...`, com PostgreSQL e Redis locais isolados, aprovada; `go build -mod=readonly ./apps/api/...` aprovado.
- Frontend: `npm run build` e `npm run lint` aprovados; permanecem avisos de lint preexistentes.
- `git diff --check` aprovado nos dois repositórios.
- APIs reais não foram usadas para compras, mensagens ou escritas durante os testes.
- Relatórios com dados de clientes ficaram fora do commit porque os repositórios são públicos.
