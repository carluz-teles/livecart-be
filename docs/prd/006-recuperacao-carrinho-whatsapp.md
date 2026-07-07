# PRD 006 - Recuperacao de Carrinho via WhatsApp

**Status:** 🟢 Em implementacao
**Prioridade:** P0
**Estimativa:** 4 sprints

---

## 1. Visao Geral

### Problema
Carrinhos criados durante as lives expiram sem pagamento e a receita se perde.
Hoje o unico toque pos-abandono e o lembrete pre-expiracao via Instagram DM,
que depende da janela de 24h da Meta — quase sempre fechada quando o carrinho
expira. Nao existe nenhum mecanismo de recuperacao pos-expiracao.

### Solucao
Canal WhatsApp via **Twilio (BSP oficial da Meta)** usando o **numero do
proprio lojista**, com dois fluxos:
1. **Lembrete pre-expiracao**: continua no Instagram; se a janela de 24h
   fechou, faz fallback para WhatsApp.
2. **Recuperacao pos-expiracao**: sempre via WhatsApp — regenera o checkout
   (fluxo `RegenerateCheckout` existente) e envia novo link por template.

### Resultado Esperado
- "R$ X de receita recuperada esse mes" no dashboard
- Taxa de recuperacao: % de carrinhos expirados que voltam e pagam
- Zero risco de banimento do numero do lojista (API oficial)

---

## 2. Decisoes de Arquitetura (registro)

| Decisao | Escolha | Motivo |
|---------|---------|--------|
| Provedor | Twilio (BSP oficial) | Sem App Review proprio na Meta; pay-per-use sem mensalidade; Senders API GA registra numero proprio do lojista via OTP |
| Evolution API / Baileys | ❌ Descartado | Onda de banimentos massiva desde jan/2026 (novos ToS Meta); ban permanente e sem recurso; numero do lojista e ativo critico |
| Outros BSPs | 360dialog fica como opcao futura | Zero markup + modelo ISV; migrar quando volume justificar €49/numero/mes |
| Modelo de contas | 1 subaccount Twilio por loja | Padrao ISV Twilio: 1 subaccount = 1 WABA; isolamento de billing/uso |
| Numero | Do proprio lojista (non-Twilio number) | Verificacao por OTP SMS/ligacao; suporta Coexistence com o app Business |
| Billing de mensagens | Custo nosso (conta master), fair use nos planos | Tarifa Meta + ~US$0,005 Twilio/msg; recuperacao gera GMV (alinhado com % dos planos) |
| Templates | Default pre-aprovado clonado por loja no onboarding | Elimina fricao de aprovacao Meta para o lojista comecar |

---

## 3. Regras de Canal por Mensagem

| Mensagem | Momento | Canal |
|----------|---------|-------|
| Carrinho criado / item adicionado | Durante a live | Instagram (sem mudanca) |
| Lembrete de expiracao | Pre-expiracao (X min antes) | Instagram → fallback WhatsApp se janela 24h fechada |
| **Recuperacao de carrinho** | Pos-expiracao | **Sempre WhatsApp** (janela IG praticamente nunca aberta) |
| Waitlist / pagamento / envio / entrega | — | Como hoje (IG / e-mail) |

Fallback do lembrete: `notifier_instagram` tenta normal; se a Meta retornar
erro de janela expirada E o cliente tem telefone+consent E a loja tem WhatsApp
conectado → dispara template WhatsApp. `notification_log` registra a cadeia
(`ig_failed_window → whatsapp_sent`).

---

## 4. Interacao com a Expiracao do Checkout

Contexto verificado no codigo:
- `expires_at` e setado quando o cliente inicia o checkout
  (`checkout/service.go:356`, janela de `GetStoreCartExpirationMinutes`) e
  quando o evento termina (`live/repository.go:772`), com override por evento.
- A expiracao e **lazy** (endpoints rejeitam `status == "expired"`; sweep
  inline so para waitlist via `ExpireNotifiedWaitlistSweep`).
- Carrinho expirado → estoque liberado → waitlist pode consumir.

Regras da recuperacao:
1. **Nunca estender expiracao para enviar mensagem.** Pre-expiracao = mesmo
   link; pos-expiracao = deixa expirar e regenera depois.
2. **Nao fura a fila da waitlist.** `RegenerateCheckout` re-reserva sob as
   regras normais; pode voltar parcial. Se voltar zero itens, nao envia.
3. **Worker e o "expirador ativo"**: varre `expires_at < now AND status NOT IN
   (paid, ...)` — nao filtra por `status == 'expired'` (lazy, pode nao estar
   materializado). Padrao de codigo: `coupon/expirer_worker.go`.
4. **Corridas**: re-checa status no momento do envio; envia so apos commit da
   regeneracao.
5. **Fim de evento**: respeitar `close_cart_on_event_end`; toggle por loja
   "recuperar carrinhos de eventos encerrados".

---

## 5. Requisitos

### 5.1 Funcionais

| ID | Requisito | Prioridade |
|----|-----------|------------|
| RF01 | Lojista conecta o proprio numero WhatsApp (subaccount + sender OTP) | P0 |
| RF02 | Template default de recuperacao provisionado no onboarding | P0 |
| RF03 | Worker de recuperacao pos-expiracao (regenera + envia) | P0 |
| RF04 | Consent LGPD no checkout (telefone + checkbox) | P0 |
| RF05 | Opt-out por resposta (SAIR/PARAR) | P0 |
| RF06 | Fallback do lembrete IG → WhatsApp | P1 |
| RF07 | Editor de template custom com status de aprovacao Meta | P1 |
| RF08 | Metricas: receita recuperada, taxa de recuperacao | P1 |
| RF09 | Settings por loja: delay, max tentativas, quiet hours, toggle | P1 |

### 5.2 Regras de Envio

- Consent obrigatorio (telefone + checkbox no checkout)
- Delay pos-expiracao configuravel (default 30min)
- Max tentativas: default 1 (opcional 2a em 24h)
- Quiet hours: 21h–8h (nao envia; agenda para proxima janela)
- Dedupe por `cart_id + tipo` no notification_log

---

## 6. Arquitetura Tecnica

### 6.1 Backend (apps/api)

- **Integration**: provider `twilio_whatsapp` (type `communication`) em
  `internal/integration/providers/communication/twilio.go`. Credenciais
  criptografadas (`lib/crypto`): subaccount SID + auth token. Metadata: WABA
  ID, sender SID, numero, quality rating. Lifecycle existente
  (`pending_auth → active → error/disconnected`).
- **Endpoints**: `POST /stores/{id}/integrations/whatsapp` (onboarding),
  `GET .../whatsapp/status`, `POST .../whatsapp/test`.
- **Webhook**: `POST /api/webhooks/twilio/{integrationId}` — status callbacks
  (sent/delivered/read/failed) + inbound (opt-out). Validar
  `X-Twilio-Signature`.
- **Notification**: canal `whatsapp`, tipo `cart_recovery`;
  `notifier_whatsapp.go` ao lado do `notifier_instagram.go`. Envio via
  Content API (`ContentSid` + `ContentVariables`) — business-initiated exige
  template.
- **Worker**: `RecoveryWorker` (ticker + stop channel + WaitGroup, padrao
  `coupon/expirer_worker.go`), wired no `cmd/http-server/main.go`.
- **Checkout**: campos `whatsapp_consent` + timestamp no cart/customer.

### 6.2 Frontend (livecart-fe)

- Integracoes: card WhatsApp (wizard de conexao, status, quality, teste)
- Comunicacoes: template "Recuperacao de carrinho" com badge de aprovacao
- Settings: delay, tentativas, quiet hours, toggle
- Dashboard: card "Receita recuperada"

### 6.3 Config

```
TWILIO_ACCOUNT_SID=   # conta master (Console -> Account Info)
TWILIO_AUTH_TOKEN=
```

---

## 7. Implementacao

| Sprint | Entrega | Valor |
|--------|---------|-------|
| 1 | Migrations + provider Twilio + notifier + webhook + envio de teste | Canal funciona fim-a-fim com loja piloto |
| 2 | Onboarding self-service (subaccount + sender + template default) + card FE | Lojista conecta sozinho |
| 3 | Worker de recuperacao + consent checkout + fallback lembrete + settings | A feature gerando receita |
| 4 | Editor template custom + atribuicao/metricas | O numero que vende a feature |

### Atribuicao de venda recuperada
Pedido pago ate 48h apos mensagem de recuperacao do mesmo carrinho →
`recovered = true`. Integra com PRD 005 (atribuicao de receita).

---

## 8. Dependencias Externas

- [x] Conta Twilio (Account SID + Auth Token no .env)
- [ ] Cadastro no WhatsApp ISV/Tech Provider Program (Twilio Console) — em andamento
- [ ] Numero de teste para validacao fim-a-fim
- [ ] Template default aprovado pela Meta (submetido automaticamente no onboarding de cada loja)

---

## 9. Status da Implementacao (jul/2026)

Todas as 4 sprints implementadas. Commits: 9b1ad69 (S1), 837a571 + a6088a8-fe
(S2), f26e3c5 + c42fb53-fe (S3), sprint 4 no commit seguinte.

Decisoes tomadas durante a implementacao:
- Lembrete continua IG-first; fallback WhatsApp quando a janela de 24h fecha
  (decisao de produto). Recuperacao pos-expiracao e sempre WhatsApp.
- O fallback usa o MESMO template aprovado da recuperacao (business-initiated
  nao aceita texto livre); template dedicado de lembrete fica para depois.
- Settings (toggle/delay) + stats de receita recuperada vivem no dialog do
  WhatsApp em Integracoes — nao houve card no dashboard ainda.
- max_attempts efetivo e 1 (NOT EXISTS no sweep); multi-tentativa e follow-up.
- Sweep tem piso de 7 dias para nao ressuscitar carrinhos antigos no launch.

Follow-ups conhecidos:
- Editor de template custom (criar Content novo + aprovacao Meta + swap do
  content_sid) — a UI atual mostra o status de aprovacao, mas o texto enviado
  e sempre o template default provisionado no onboarding.
- Card "Receita recuperada" no dashboard principal (PRD 005).
- Segunda tentativa de recuperacao em 24h (config max_attempts ja existe).
- Validar fim-a-fim quando o cadastro ISV/Tech Provider for aprovado
  (registro de sender exige WABA via embedded signup).
