# Falha em settings/billing — Maricelia Decor

## Diagnóstico

Verificação de produção somente leitura, via Railway CLI, PostgreSQL e GET na
Stripe, em 17/09/2026:

- Assinatura local `active`, plano `pro`, intervalo `annual`, ciclo de
  17/09/2026 até 17/09/2027. Sem override manual de acesso.
- Stripe também retornou assinatura ativa e recorrência anual. A fatura está
  `paid`: subtotal de R$ 6.447,60, desconto de R$ 6.447,60, total devido e pago
  de R$ 0,00. Não houve cobrança nessa fatura.
- O backend respondeu **200** a `/billing/subscription` às **10:49:31 e
  10:50:56 BRT**, em aproximadamente 2 ms. A falha ocorre na renderização do
  frontend, não como erro HTTP desse endpoint.
- `subscriptions.billing_interval` existia no banco, mas era descartado por
  `rowToSubscription` e não existia em `SubscriptionState`. A tela usava
  `BILLING_INTERVAL_LABELS[sub.billingInterval].toLowerCase()` e lançava
  `TypeError: Cannot read properties of undefined (reading 'toLowerCase')`.
- O problema alcançava qualquer assinatura Pro que entrasse nesse trecho da
  tela, inclusive mensal e semestral; não depende do percentual do cupom.

## Correção

O backend preserva o intervalo na entidade e o devolve em `billingInterval`,
tanto no endpoint de assinatura quanto no snapshot usado por `/users/sync`.

O frontend tolera intervalos ausentes/desconhecidos durante um deploy parcial,
sem inventar um intervalo ou valor de cobrança. Falhas de consulta passam a
mostrar opção de tentar novamente; dados anteriores ficam identificados como
possivelmente desatualizados.

O valor fixo do catálogo agora aparece como **valor de tabela**, com orientação
para conferir descontos e valores finais no portal. O painel financeiro deixa
de apresentar esse valor como previsão da próxima fatura. Esta correção não
implementa uma nova consulta de descontos da Stripe na tela.

Não há migration, alteração em dados de produção, cobrança, mudança de cupom
ou mudança nas regras de acesso. Publicar backend e frontend resolve o contrato;
nenhum reparo manual na assinatura da Maricelia é necessário para este erro.

## Validação

- Teste de integração do endpoint falhou antes do fix nos três intervalos:
  `billingInterval = <nil>`.
- Depois do fix, suíte de billing e usuário passou com PostgreSQL local e `-race`.
- Oito testes de frontend passaram: intervalos válidos, ausente, nulo, vazio,
  desconhecido e tipos inválidos. São testes da apresentação, não E2E autenticados.
- `go build ./...`, `npm run build` e `git diff --check` passaram.

Branches para revisão: BE `fix/billing-subscription-interval`; FE
`fix/billing-subscription-display`. A publicação em produção depende do merge
dos PRs pelo responsável pela loja.

Referência consultada: a Stripe admite Checkout de assinatura com valor zero
após desconto, inclusive sem coletar cartão se configurado `if_required`:
[Create a Checkout Session](https://docs.stripe.com/api/checkout/sessions/create#checkout_session_create-payment_method_collection).
