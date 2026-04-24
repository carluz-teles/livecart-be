# Prompt para IA do Frontend — Integração SmartEnvios + multi-provider de frete

Copie tudo abaixo e cole na IA que mexe no frontend LiveCart. Troque o que estiver entre `{{...}}` se precisar.

---

## Contexto

O backend do LiveCart passou a suportar **múltiplos providers de frete simultâneos** por loja. Hoje havia só **Melhor Envio** (OAuth); agora existe também **SmartEnvios** (autenticação por token estático) — e o design já prevê novos providers no futuro.

No checkout, o cliente deve ver **todas as opções** dos providers ativos misturadas em uma única lista (ex.: PAC/SEDEX do Melhor Envio + Jadlog Package da SmartEnvios lado a lado).

Suas tarefas: (1) atualizar o painel admin pra permitir conectar/rotacionar SmartEnvios; (2) atualizar a tela de "Frete" (admin) pra listar **ambas** as integrações; (3) atualizar o checkout público pra consumir o novo contrato agregado; (4) adicionar as telas admin de pós-pedido (criar envio, anexar NFe, emitir etiqueta, ver rastreio).

## Mudanças contratuais **breaking** no backend (atenção)

1. `serviceId` virou **string opaca** (antes era `int`). Vale para cotação, seleção e persistência no cart. SmartEnvios retorna ObjectId (ex.: `"5eb9b31e1097eb6cdf922d04"`), Melhor Envio devolve int-como-string (ex.: `"3"`).
2. Todas as opções de frete agora carregam um campo `provider` (`"melhor_envio"` | `"smartenvios"`). Ecoe esse valor na seleção.
3. `CartShippingSelection` ganhou `provider: string` — aparece no GET `/api/public/checkout/:token`.

Se o frontend tem types TypeScript espelhando esses DTOs, atualize:

```ts
type ShippingQuoteOption = {
  id: string;                 // was number
  provider: string;           // NEW — "melhor_envio" | "smartenvios" | ...
  service: string;
  carrier: string;
  carrierLogoUrl?: string;
  priceCents: number;
  realPriceCents: number;
  deadlineDays: number;
  available: boolean;
  error?: string;
};

type CartShippingSelection = {
  provider: string;           // NEW
  serviceId: string;          // was number
  serviceName: string;
  carrier: string;
  costCents: number;
  realCostCents: number;
  deadlineDays: number;
  freeShipping: boolean;
};
```

## Endpoints públicos (checkout) — atualizados

### `POST /api/public/checkout/:token/shipping-quote`
Request:
```json
{ "zipCode": "20000-000" }
```
Response (200):
```json
{
  "quotedAt": "2026-04-24T14:03:21Z",
  "freeShipping": false,
  "options": [
    { "id": "3",  "provider": "melhor_envio",
      "service": "PAC", "carrier": "Correios",
      "carrierLogoUrl": "https://...", "priceCents": 2450,
      "realPriceCents": 2450, "deadlineDays": 9,
      "available": true, "error": "" },
    { "id": "5eb9b31e1097eb6cdf922d04", "provider": "smartenvios",
      "service": "Jadlog Package", "carrier": "Jadlog",
      "carrierLogoUrl": "", "priceCents": 1064,
      "realPriceCents": 1064, "deadlineDays": 2,
      "available": true, "error": "" }
  ]
}
```
Quando a opção não está disponível: `available: false` + `error` preenchido. **UI deve mostrar o erro em cinza/disabled**, não sumir.

### `PUT /api/public/checkout/:token/shipping-method`
Request:
```json
{
  "provider": "smartenvios",
  "serviceId": "5eb9b31e1097eb6cdf922d04",
  "zipCode": "20000-000"
}
```
Response (200):
```json
{
  "shipping": {
    "provider": "smartenvios",
    "serviceId": "5eb9b31e1097eb6cdf922d04",
    "serviceName": "Jadlog Package",
    "carrier": "Jadlog",
    "costCents": 1064,
    "realCostCents": 1064,
    "deadlineDays": 2,
    "freeShipping": false
  },
  "summary": { "subtotal": 50000, "shippingCost": 1064, "total": 51064, "totalItems": 2, "hasShippingQuote": true }
}
```
Quando a loja tem **só um** provider ativo, `provider` pode ser omitido — o backend infere. Mande sempre que tiver a informação, fica mais robusto.

### `GET /api/public/checkout/:token`
Igual a antes, mas o objeto `shipping` (quando presente) inclui `provider`.

## Endpoints admin (novos) — precisam de UI

Todos no grupo autenticado `/api/v1/stores/:storeId/integrations`:

### Conectar SmartEnvios (token estático, sem OAuth)
```
POST /integrations/shipping/smartenvios/connect
Body: { "token": "<TOKEN_DO_EMBARCADOR>", "env": "production" | "sandbox" }
```
Valida o token em tempo real (chama `GET /quote/services` da SmartEnvios). Se inválido, retorna 422 com mensagem explícita — exiba no formulário. Se válido, cria ou atualiza a integração com `status=active`. Use esse endpoint **também para rotação** do token (basta reenviar).

### Listar serviços habilitados
```
GET /integrations/shipping/:provider/carriers
```
`:provider` = `melhor_envio` | `smartenvios`. Retorna `[{ serviceId, service, carrier, carrierLogoUrl, insuranceMaxCents }]`. Use na tela de configuração de frete pra mostrar "Jadlog Package, Gollog, Sedex, PAC..." que o embarcador tem contrato.

### Criar envio (pós-pagamento, admin)
```
POST /integrations/shipping/:provider/shipments
Body: {
  "quoteServiceId": "<id da cotação salva no carrinho>",
  "externalOrderId": "<nosso order id>",
  "invoiceKey": "<chave NFe, 44 chars, opcional>",
  "sender":  { "name": "...", "document": "...", "zipCode": "...", "street": "...", "number": "...", "neighborhood": "...", "complement": "", "phone": "", "email": "", "observation": "" },
  "destiny": { "name": "...", "document": "...", "zipCode": "...", "street": "...", "number": "...", "neighborhood": "...", "complement": "", "phone": "", "email": "" },
  "items": [
    { "id": "SKU-1", "name": "Camiseta", "quantity": 1,
      "unitPriceCents": 12990, "weightGrams": 500,
      "heightCm": 10, "widthCm": 20, "lengthCm": 20 }
  ],
  "volumeCount": 1,
  "observation": "Entregar somente ao destinatário"
}
```
Response (201):
```json
{
  "providerOrderId": "d2889bac-...", "providerOrderNumber": "6355936",
  "trackingCode": "SM806381523458D0",  "invoiceId": "325424-...",
  "status": "pending", "statusRawCode": 1, "statusRawName": "Pedido em Aberto",
  "createdAt": "2022-01-06T20:13:27.523Z"
}
```

### Anexar NFe por chave
```
POST /integrations/shipping/:provider/shipments/:shipmentId/invoice
Body: { "invoiceKey": "NFe35240254116...", "invoiceKind": "nfe" }
```

### Upload XML da NFe (multipart)
```
POST /integrations/shipping/:provider/shipments/:shipmentId/invoice-xml
Content-Type: multipart/form-data
Field: file=@arquivo.xml
```

### Gerar etiquetas
```
POST /integrations/shipping/:provider/labels
Body: {
  "providerOrderIds": ["d2889bac-..."] | "trackingCodes": ["SM..."] | "invoiceKeys": ["NFe..."] | "externalOrderIds": ["123"],
  "format": "pdf" | "zpl" | "base64",
  "documentType": "label_integrated_danfe" | "label_separate_danfe"
}
```
Response:
```json
{
  "labelUrl": "https://smartenvios.com/pdf/SM...",
  "tickets": [
    { "providerOrderId": "...", "trackingCode": "SM...",
      "publicTracking": "https://v1.portal.smartenvios.com/tracking/SM...",
      "volumeBarcodes": ["SMP0538583001", "SMP0538583002"] }
  ]
}
```

### Puxar rastreio (on-demand, sem webhook)
```
POST /integrations/shipping/:provider/tracking
Body (exatamente UM campo): {
  "providerOrderId"?, "externalOrderId"?, "invoiceKey"?, "trackingCode"?
}
```
Response:
```json
{
  "trackingCode": "SM3028078737SD5", "carrier": "Total Express", "service": "Total Express",
  "currentStatus": "delivered",
  "events": [
    { "status": "in_transit", "rawCode": 4, "rawName": "Recebido Base SmartEnvios",
      "observation": "Pedido recebido na Base.", "eventAt": "2021-01-20T21:14:23.380Z" },
    { "status": "delivered", "rawCode": 7, "rawName": "Entrega Realizada",
      "observation": "...", "eventAt": "2021-01-27T16:29:30.243Z" }
  ]
}
```

### Enum `status` / `currentStatus`
Normalizado do lado do backend — todos os providers caem nesse mesmo enum:
`unknown`, `awaiting_invoice`, `pending`, `pending_pickup`, `pending_dropoff`,
`awaiting_pickup`, `in_transit`, `out_for_delivery`, `delivered`,
`delivery_issue`, `delivery_blocked`, `issue`, `shipment_blocked`, `damaged`,
`stolen`, `lost`, `fiscal_issue`, `refused`, `not_delivered`,
`indemnification_requested`, `indemnification_scheduled`, `indemnification_completed`,
`returning`, `returned`, `canceled`.

Sugiro agrupar na UI em 6 buckets: *Aguardando*, *Em trânsito*, *Entregue*, *Problema*, *Devolução*, *Cancelado*.

## Telas a construir / atualizar

### 1. Admin → Integrações → **Conectar SmartEnvios** (novo card)
- Campo: token (string longa, password-like, com botão "revelar")
- Radio: ambiente (`sandbox` default em dev; `production` em prod)
- Botão "Conectar" → `POST /integrations/shipping/smartenvios/connect`
- Sucesso: mostra status *Ativo*, botões "Testar conexão" (`POST /integrations/:id/test`), "Rotacionar token" (reabre o formulário), "Desconectar" (DELETE).
- Erro (422): mostra a mensagem de validação ao lado do token.

### 2. Admin → Frete → **Visão geral** (atualizar)
Hoje imagino que lista 1 provider; precisa listar os N ativos. Para cada integração ativa (`melhor_envio`, `smartenvios`), mostrar:
- Status + logo
- Lista de serviços habilitados (`GET /integrations/shipping/:provider/carriers`)
- Botão "Testar conexão" e "Desconectar"

### 3. Checkout público → **Lista de opções de frete** (atualizar)
- Após cotação (`POST /shipping-quote`), renderize `options[]` como é hoje — **nada muda visualmente**; só a fonte do `id` (agora string) e a presença do `provider`.
- Ordenar por `priceCents` ASC e exibir opções `available=false` no fim com ícone cinza + `error`.
- Na seleção, chamar `PUT /shipping-method` com `{ serviceId, provider, zipCode }`.
- Quando houver múltiplos providers com nomes de serviço idênticos (improvável mas possível), mostrar badge sutil com o nome do provider abaixo do serviço (ex.: *via Melhor Envio*).

### 4. Admin → Pedido pago → **Painel de logística** (novo)
Quando um pedido estiver `paid`, renderize um card "Logística" com:
- Se ainda não tem envio criado: botão "Criar envio" → abre modal pré-preenchido com endereço/itens/cotação escolhida → `POST /shipments`.
- Depois de criado: exibir `trackingCode`, status traduzido, `providerOrderNumber` com link para `publicTracking`.
- Botões:
  - "Anexar NFe" → formulário de chave NFe → `POST /invoice`.
  - "Upload XML" → input file → `POST /invoice-xml`.
  - "Gerar etiqueta" → `POST /labels` → abrir `labelUrl` em nova aba.
  - "Atualizar rastreio" → `POST /tracking` → preencher timeline na tela.

## Dicas de implementação

- **IDs opacos**: não tente parsear `serviceId` — use como string, sempre. Persistir em estado/URL deve ser direto.
- **Provider default no checkout**: se `options[]` tem só uma entrada de um único provider, pode-se omitir `provider` no PUT. Com múltiplos, sempre manda.
- **Aggregation na lista**: mostre todas as opções, mesmo que 5 sejam de `melhor_envio` e 3 de `smartenvios`. Não tente agrupar por carrier — o usuário vai querer ver tudo.
- **Erro parcial**: se um provider falhar (timeout SmartEnvios), o backend ainda retorna as opções do outro. Não há sinalização explícita — se `options[]` vier vazio, o backend devolve 422 (`"nenhum provider de frete retornou opções"`).
- **Status pós-criação**: após `POST /shipments`, o status mais comum é `pending` (code 1 / "Pedido em Aberto"). Só vira `in_transit` depois que a transportadora coletar.
- **Feature flag**: se quiser esconder o SmartEnvios em produção até estar testado, use uma flag no backend/env (`SMARTENVIOS_ENABLED`?). No frontend nada precisa — se a integração não existir pro storeID, nada aparece.

## Observações / pendências

- **Webhooks SmartEnvios não estão implementados na v1.** Tracking é via pull (`POST /tracking`) — botão "atualizar" no admin. Se o usuário quiser ver evolução em tempo real no futuro, tem que abrir ticket com `dev@smartenvios.com` sobre autenticidade de webhook e aí implementar.
- **SmartEnvios não tem endpoint de cancelamento oficial**. Se precisar cancelar, faça via portal deles. Se quiser expor botão de cancelar no admin, ele vai retornar erro — deixe desabilitado para `provider === "smartenvios"` e com tooltip explicando.
- **`unitPriceCents` → reais inteiros**: ao criar shipment, o backend arredonda o valor em reais (SmartEnvios só aceita integer reais, não centavos). Isso é uma limitação da API deles; o valor fiscal real vai na NFe anexada. Não precisa ajustar UI.
