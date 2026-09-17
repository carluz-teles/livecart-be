# Tiny: disponibilidade e entrega de webhooks — 17/09/2026

## Evidências do incidente

Investigação de produção somente leitura, com Railway CLI, consultas SQL e GETs
na Tiny. Nenhuma alteração de estoque, integração ou pedido foi executada em
produção nesta investigação.

- Canto da Art, keyword **1537**, SKU **MD2026**, Tiny **847743332**: o SYNC manual
  de 17/09 às **09:01 BRT** leu físico/reservado/disponível iguais a zero e aplicou
  zero no LiveCart. As leituras posteriores dos dois sistemas confirmaram zero.
  Não foi possível recuperar o saldo local exato anterior a esse SYNC.
- Em uma amostra dirigida de 12 produtos ativos com saldo positivo e atualização
  antiga, **keyword 1065 / SKU 91786001 / Tiny 835537459** tinha **2 no LiveCart e
  0 disponível na Tiny**. Os outros 11 saldos conferiram. Essa seleção não estima
  a proporção de divergências no catálogo inteiro.
- O lojista confirmou que a Tiny desabilitou os webhooks por excesso de erros.

### Linha do tempo da indisponibilidade (BRT)

| Data/hora | Evidência |
| --- | --- |
| 13/09 15:14 | Último `webhookLastPingAt` persistido. Não é necessariamente a última entrega recebida. |
| 13/09 16:55 até 23:09 | 21 falhas `failed to store webhook event`, com PostgreSQL em recovery (`57P03`). |
| Até 13/09 23:09:48 | O trecho recuperado dos logs contém 113 respostas HTTP 200 a entregas Tiny, incluindo entregas cuja persistência falhou. |
| 14/09 00:30:44 | Encerramento da instância anterior do backend. |
| 14/09 00:30–00:31 | Novo deploy falhou no startup/migrations porque o banco continuava em recovery. |

O esgotamento do volume e a indisponibilidade do backend são compatíveis com a
desativação relatada. Não recuperamos os códigos/horários das 20 tentativas
específicas que a Tiny contabilizou; não é possível atribuí-las individualmente.
Uma resposta de gateway quando não há backend disponível também não aparece
como resposta HTTP nos logs da aplicação.

A documentação da Olist descreve remoção da URL após 20 tentativas malsucedidas
da mesma notificação, com intervalos crescentes:
[Por que minha URL do Webhook foi removida?](https://ajuda.olist.com/gestao-de-integracoes/por-que-minha-url-do-webhook-foi-removida).

## Correções no código

1. **Confirmação durável de produto/estoque Tiny.** O receptor grava um comando
   compacto no `event_outbox` antes de responder 200. O consumidor usa a fila
   existente e suas retentativas. Falhas posteriores de API/rate limit não
   mudam o ACK já enviado. Se nem o comando puder ser gravado, responde 503:
   responder 200 nessa situação descartaria a oportunidade de reentrega.
2. **Leitura de estoque disponível.** Notificações de estoque consultam somente
   o endpoint de estoque, sem baixar novamente todos os detalhes do produto.
   O saldo do payload é apenas um sinal para atualização. Mantida a compensação
   das reservas locais ainda não refletidas no ERP.
3. **Proteção contra consultas concorrentes.** Aplicar um saldo também avança
   `erp_seq`. Uma leitura iniciada antes de outra atualização não pode sobrescrever
   o saldo mais recente. O SYNC manual captura essa versão antes do HTTP.
4. **Falha explícita quando o saldo não foi aplicado.** Estoque desconhecido não
   vira zero; leitura invalidada não retorna sucesso. O SYNC manual retorna 409
   com orientação de tentar novamente quando há conflito de versão.
5. **Variações.** Atualizar metadados de variantes existentes não sobrescreve o
   estoque. Seus saldos passam pela mesma leitura de disponibilidade e trava.
6. **Recuperação periódica.** Uma varredura consulta até 10 produtos por conta
   Tiny a cada minuto, sob o limitador compartilhado e uma concessão no banco
   que impede multiplicação do lote entre réplicas. Cada conta tem orçamento de
   40 segundos por rodada. Uma falha individual não interrompe os demais SKUs.
7. **Saúde e observabilidade.** Timestamp de webhook de estoque persistido junto
   ao comando; ping genérico atualizado com merge atômico de metadados. O lookup
   de loja usado apenas para enriquecer logs tem timeout de 500 ms.

Não há deduplicação permanente por produto ou saldo: mudanças A → B → A são
legítimas. Reentregas podem gerar nova consulta, mas o consumidor sempre busca
o saldo atual. Comandos vinculados a uma integração removida/substituída são
ignorados.

Referência de estoque:
[Obter o estoque de um produto](https://api-docs.erp.olist.com/api-reference/estoque/obter-o-estoque-de-um-produto).

## Escopo e limites

- A fila nova recebe **produto e estoque Tiny**. Webhooks de pedido/nota Tiny
  mantêm o fluxo anterior; não foram todos migrados nesta correção.
- O espelho de estoque, SYNC manual, metadados de variantes e middleware são
  compartilhados. Essas proteções também atingem caminhos usados por outros
  ERPs. A nova varredura e o novo recebimento enfileirado são específicos da Tiny.
- A migration **157** é aditiva: `erp_stock_sync_state` mantém no máximo uma
  linha por produto, com datas de tentativa/sucesso e exclusão em cascata quando
  o produto é removido. Não armazena payloads nem histórico crescente.
- O backstop é gradual. Para 1.532 produtos, 10 consultas por minuto exigem pelo
  menos cerca de **154 minutos** para uma rodada completa; erros, concorrência
  de API e cooldown podem ampliar esse tempo. Não substitui webhooks ativos nem
  garante estoque instantâneo entre canais independentes.
- Responder 200 exige conseguir receber e persistir. Não resolve indisponibilidade
  do servidor ou do banco que sustenta a fila. Os logs antigos já mostram o risco
  de responder 200 apesar de uma falha de persistência.

## Validação local

- Reproduzidos contra o código anterior: snapshot antigo sobrescrevendo zero,
  SYNC manual reoferecendo unidade reservada durante a consulta e estoque
  desconhecido aceito como zero. Os três cenários falharam na versão anterior.
- Novos testes de integração com PostgreSQL 17 e `-race` passaram: persistência
  antes do ACK, falha de gravação sem falso ACK, reentrega, saldo disponível,
  concorrência, variantes, rotação e exclusão entre réplicas, integração
  substituída, saúde e preservação atômica de metadados.
- Suítes de produto, productgroup, eventos, ERP/providers, ERP e httpx passaram.
  A suíte integration passou no primeiro conjunto; uma segunda execução ampla
  encontrou timeout no teste existente
  `TestBlingRateBudgetSharesReadsWritesAndCooldownAcrossReplicas`. Sua repetição
  isolada passou. Registrar a instabilidade, sem atribuí-la à nova lógica.
- `go build ./...` e `git diff --check` passaram após as últimas alterações.
- A verificação de convenções ainda reporta 10 usos de erros HTTP sem código em
  funções preexistentes de Instagram compartilhado. Nenhum uso novo permanece
  apontado por essa verificação; esse débito não foi alterado neste trabalho.

## Retomada operacional

O GET de validação da URL abaixo respondeu 200 durante a investigação:

`https://api.livecart.com.br/api/webhooks/tiny/a5403331-afd4-40ff-9bbe-4aa6f5322aee`

O lojista pode reativar essa URL no aplicativo Tiny atual. Esse GET verifica a
rota; não comprova que uma notificação real já voltou a chegar. É preciso
acompanhar uma nova entrega após a reativação. Um SYNC de catálogo autorizado
pelo lojista pode recuperar o acúmulo atual sem esperar a varredura gradual.

Após publicar o código e executar a migration 157, acompanhar:

- `Tiny product webhook not persisted`: falha anterior ao ACK, exige atenção à
  disponibilidade do banco/receptor.
- `ERP available stock reconciled`: saldo anterior, disponível no ERP e saldo
  admissível aplicado.
- `Tiny stock recovery deferred`: motivo de uma leitura adiada.
- `Tiny stock recovery batch finished`: quantidade selecionada e conferida.

Branch isolada de implementação: `fix/tiny-stock-availability`, baseada em main.
Este documento não registra publicação nem execução da migration em produção.
