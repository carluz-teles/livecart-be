# Tiny: confirmação do checkout e parcelas idempotentes

Correção de contenção do incidente `79b126cd-2233-4c85-8ad5-93c58cb1ddc5`.

## Comportamento

- A confirmação Tiny lê o pedido e compara cliente (nome, documento, e-mail e
  telefone informados), endereço, frete e total com o checkout pago.
- Divergências retornam erro para a finalização existente, que registra falha e
  mantém a pendência. A venda paga no LiveCart permanece paga; o pedido não é
  automaticamente aprovado como se os dados tivessem sido sincronizados.
- Quando os dados já correspondem, grava as parcelas do livro de pagamentos e
  relê para confirmar. HTTP 204 sozinho não comprova a gravação.
- Parcelas iguais não geram outro PUT; a comparação ignora ordem e preserva a
  data de vencimento de saldo ainda não pago. Pagamentos e descontos continuam
  comparando valor, observação e data.
- O desconto comercial já presente no total da Tiny é descontado da
  recomposição, evitando contá-lo uma segunda vez.
- Pedido com nota fiscal não recebe regravação de parcelas. A presença da nota
  é conferida tanto na recomposição quanto imediatamente antes da escrita.

## Limitação que permanece

**Não foi implementado o preenchimento automático de frete e cliente em um
pedido Tiny existente.** O contrato v3 de atualização não documenta esses
campos. A mudança torna essa falha visível e impede a falsa confirmação, mas
pedidos divergentes ainda exigem conciliação. Não é uma entrega completa da
solicitação de envio automático desses dados.

Fontes oficiais consultadas em 10/09/2026:

- https://api-docs.erp.olist.com/api-reference/pedidos/atualizar-pedido
- https://api-docs.erp.olist.com/api-reference/pedidos/obter-pedido

O GET usa `enderecoEntrega.numero`, enquanto a criação existente usa outro
contrato. Nenhum campo não documentado foi acrescentado ao PUT. Não se cancela
nem recria pedido, não se estorna estoque/contas e não se cancela nota fiscal.

Para concluir a automação, é necessário homologar um fluxo suportado de edição
com a Tiny ou adaptar a criação inicial preservando reserva e identidade, em
conta de testes. Não se deve experimentar isso com pedidos faturados do cliente.

## Validação

- Reprodução HTTP da falta de frete, cliente divergente e nota emitida: nenhuma
  escrita no pedido e retorno de erro, sem falsa confirmação.
- Checkout correspondente: parcelas gravadas e verificadas por leitura.
- PUT ignorado com HTTP 204: confirmação recusada após releitura.
- Três chamadas iguais com pagamento e desconto: um único PUT.
- Nota fiscal: zero PUTs; teste do serviço também preserva as parcelas.
- Pacotes de integração, ERP e provedores passaram com PostgreSQL descartável
  e `-race`. A suíte `go test ./apps/api/...` passou com PostgreSQL descartável.
  Build `go build -buildvcs=false ./apps/api/...` passou; a opção desabilita
  apenas a identificação VCS do binário de verificação, devido a um `.git`
  alheio em `/tmp` que interferiu na descoberta do worktree pelo Go.

Produção foi apenas consultada na investigação. Este hotfix é separado do
trabalho Nuvemshop e não inclui suas migrations ou funcionalidades.
