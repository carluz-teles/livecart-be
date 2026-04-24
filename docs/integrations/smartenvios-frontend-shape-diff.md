# Shape diff — backend shipments persistence + OrderDetail enrichment

Follow-up ao prompt anterior. Backend fechou os 3 gaps que você flaggou no PR da Fase 4. Commit: `aaf9a74` em `origin/main`. Migration aplicada em dev (`schema_migrations.version = 52`).

Este doc dá exatamente o que o `OrderDetail` retorna agora, pra você remover os fallbacks defensivos na `OrderLogistics`.

## 1. Status dos caveats

| Caveat do front | Status | O que mudou no backend |
|-----------------|--------|------------------------|
| `OrderDetail.customer` / `shippingAddress` / `shipping` / `shipment` speculative | ✅ Fechado | Os 4 blocos agora são serializados (opcionais, ver regras abaixo) |
| Item dimensions 15×15×15 cm fallback | ✅ Fechado | `GET /orders/:id` agora devolve `weightGrams`/`heightCm`/`widthCm`/`lengthCm`/`packageFormat` por item |
| `publicTrackingUrl` speculative | ✅ Fechado | Vem populado em `shipment.publicTrackingUrl` quando a etiqueta já foi gerada |

## 2. Endpoint — contrato novo

```
GET /api/v1/stores/:storeId/orders/:id
```

Todos os campos "antigos" (`id`, `items`, `status`, etc.) continuam idênticos. Os **blocos novos são opcionais** — ver regras de presença no final.

### Response — exemplo completo

```json
{
  "id": "8e9435...",
  "liveSessionId": "...", "liveTitle": "...", "livePlatform": "instagram",
  "customerHandle": "@joao", "customerId": "...",
  "status": "checkout", "paymentStatus": "paid",
  "totalItems": 2, "totalAmount": 29990,
  "paidAt": "...", "createdAt": "...", "expiresAt": null,

  "items": [
    {
      "id": "...", "productId": "...", "productName": "Camiseta Preta",
      "productImage": "https://...", "keyword": "camiseta-p",
      "size": "M", "quantity": 1,
      "unitPrice": 14990, "totalPrice": 14990,

      "weightGrams": 500,
      "heightCm": 10,
      "widthCm": 25,
      "lengthCm": 30,
      "packageFormat": "box"
    }
  ],

  "comments": [],

  "customer": {
    "name": "João Silva",
    "email": "joao@example.com",
    "document": "12345678900",
    "phone": "11999998888"
  },

  "shippingAddress": {
    "zipCode": "01310-100",
    "street": "Avenida Paulista",
    "number": "1000",
    "complement": "Apto 42",
    "neighborhood": "Bela Vista",
    "city": "São Paulo",
    "state": "SP"
  },

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

  "shipment": {
    "id": "a1b2c3d4-...",
    "provider": "smartenvios",
    "providerOrderId": "d2889bac-...",
    "providerOrderNumber": "6355936",
    "trackingCode": "SM806381523458D0",
    "publicTrackingUrl": "https://v1.portal.smartenvios.com/tracking/SM806381523458D0",
    "invoiceKey": "NFe35240254116...",
    "invoiceKind": "nfe",
    "labelUrl": "https://smartenvios.com/pdf/SM806381523458D0",
    "status": "in_transit",
    "statusRawCode": 6,
    "statusRawName": "Em trânsito",
    "createdAt": "2026-04-24T12:00:00Z",
    "updatedAt": "2026-04-25T09:30:00Z",
    "events": [
      {
        "status": "pending",
        "rawCode": 1,
        "rawName": "Pedido em Aberto",
        "observation": "",
        "eventAt": "2026-04-24T12:00:00Z",
        "source": "poll"
      },
      {
        "status": "in_transit",
        "rawCode": 6,
        "rawName": "Em trânsito",
        "observation": "Pedido em trânsito pela transportadora.",
        "eventAt": "2026-04-25T09:30:00Z",
        "source": "poll"
      }
    ]
  },

  "store": {
    "id": "...",
    "name": "Minha Loja",
    "logoUrl": "https://...",
    "document": "12.345.678/0001-90",
    "email": "contato@minhaloja.com",
    "phone": "11933334444",
    "address": {
      "zipCode": "14170-763",
      "street": "Rua X",
      "number": "100",
      "complement": "",
      "neighborhood": "Centro",
      "city": "Ribeirão Preto",
      "state": "SP"
    },
    "shippingDefaults": {
      "packageWeightGrams": 200,
      "packageFormat": "box"
    }
  }
}
```

## 3. Regras de presença dos blocos opcionais

| Bloco | Quando aparece | Quando é `null` / ausente |
|-------|----------------|----------------------------|
| `customer` | Cliente preencheu ao menos um dos campos no checkout (name/email/document/phone) | Cliente abandonou antes de preencher |
| `shippingAddress` | Cliente confirmou o endereço no checkout (zipCode non-empty) | Sem CEP ainda |
| `shipping` | Cliente selecionou uma opção de frete (`PUT /shipping-method`) | Cliente não escolheu frete |
| `shipment` | `POST /shipments` foi chamado com sucesso | Nenhum envio criado ainda |
| `store` | **Sempre presente** em OrderDetail | — |

Itens (`items[]`) sempre trazem os campos de dimensão — podem vir com `0` quando o produto não tem dimensões cadastradas. Trate `0` como "faltando", não como zero literal.

## 4. Campos do `shipment` — o que é garantido vs opcional

`shipment.*` todos aparecem SEMPRE quando o bloco existe, mas alguns podem ser string vazia `""` quando ainda não foi preenchido pelo fluxo:

| Campo | Sempre populado? | Quando vazio |
|-------|------------------|--------------|
| `id` | ✅ | — |
| `provider` | ✅ | — |
| `providerOrderId` | ✅ | — |
| `status` | ✅ | — (começa com `pending`) |
| `createdAt` / `updatedAt` | ✅ | — |
| `providerOrderNumber` | Normalmente | `""` se o provider não devolve número |
| `trackingCode` | ✅ geralmente | `""` em raros casos onde o provider só devolve depois |
| `publicTrackingUrl` | Depois de `POST /labels` | `""` enquanto a etiqueta não for gerada |
| `invoiceKey` | Após `attach` / `upload` | `""` enquanto NFe não vinculada |
| `invoiceKind` | Junto com `invoiceKey` | `""` |
| `labelUrl` | Após `POST /labels` | `""` enquanto a etiqueta não for gerada |
| `statusRawCode` | Após 1º pull de tracking | `0` (use `rawName == ""` pra detectar) |
| `statusRawName` | Após 1º pull de tracking | `""` |
| `events[]` | Sempre é array (nunca null) | `[]` antes do 1º tracking pull |

## 5. Mudança importante no `POST /shipments`

A resposta agora inclui um campo novo:

```diff
{
  "result": {
+   "shipmentId": "a1b2c3d4-...",      // ← NEW — nosso id interno (shipments.id)
    "providerOrderId": "d2889bac-...",
    "providerOrderNumber": "6355936",
    "trackingCode": "SM806381523458D0",
    ...
  }
}
```

**Importante pro front:** todos os endpoints que recebem `:shipmentId` no path agora esperam o **`shipmentId` interno** (não o `providerOrderId`). Afeta:

- `POST /integrations/shipping/:provider/shipments/:shipmentId/invoice`
- `POST /integrations/shipping/:provider/shipments/:shipmentId/invoice-xml`

O backend resolve internamente o `providerOrderId` antes de chamar a API do provider — você só precisa persistir + enviar `shipmentId` (que agora vem no OrderDetail como `shipment.id`).

Se você vinha testando com `providerOrderId` no path, trocar por `shipment.id` (ou pelo `shipmentId` da resposta de `POST /shipments`).

## 6. Limpeza recomendada no frontend

Com base na memória [Shipping backend follow-up](memory/project_shipping_backend_followup.md) do próprio front:

1. **Remover fallback 15×15×15 cm** em `OrderLogistics/index.tsx` → `buildCreatePayload` — usar `items[].weightGrams/heightCm/widthCm/lengthCm/packageFormat` direto do OrderDetail.
2. **Tratar dimensão ausente** — quando qualquer campo for `0`, bloquear criação de envio com mensagem "produto X sem dimensões cadastradas (cadastre em Produtos)".
3. **`publicTrackingUrl`** no `IdentifierRow` — manter optional mas tirar o fallback plaintext. Quando `shipment.publicTrackingUrl === ""`, renderiza sem link (antes da etiqueta); quando populado, vira link.
4. **`collectBlockers`** — reduzir a só 2 casos reais:
   - `customer == null` ou `shippingAddress == null` → "Cliente não concluiu o checkout"
   - Algum `item.weightGrams === 0` → "Produto X sem dimensões (cadastre antes de criar envio)"
   Demais fallbacks (customer/address/shipping existem) → deleta.
5. **`shipment` types** — trocar os campos que você marcou como optional pra required quando eles sempre vêm (`id`, `provider`, `providerOrderId`, `status`, `createdAt`, `updatedAt`, `events[]`).

## 7. Tipos TypeScript sugeridos

```ts
type OrderDetail = {
  // ...existing fields...
  items: OrderItem[];
  customer: OrderCustomer | null;
  shippingAddress: ShippingAddress | null;
  shipping: ShippingSelection | null;
  shipment: Shipment | null;
  store: Store;  // always present
};

type OrderItem = {
  // ...existing...
  weightGrams: number;     // 0 = missing, not "0 grams"
  heightCm: number;
  widthCm: number;
  lengthCm: number;
  packageFormat: "box" | "roll" | "letter";
};

type Shipment = {
  id: string;                // persist as LiveCart shipmentId, NOT providerOrderId
  provider: string;
  providerOrderId: string;
  providerOrderNumber: string;
  trackingCode: string;
  publicTrackingUrl: string;  // "" before labels generated
  invoiceKey: string;
  invoiceKind: "nfe" | "dce" | "";
  labelUrl: string;           // "" before labels generated
  status: TrackingStatus;
  statusRawCode: number;      // 0 before first tracking pull
  statusRawName: string;
  createdAt: string;
  updatedAt: string;
  events: ShipmentEvent[];    // never null; [] is empty
};

type ShipmentEvent = {
  status: TrackingStatus;
  rawCode: number;
  rawName: string;
  observation: string;
  eventAt: string;
  source: "poll" | "webhook";
};

type Store = {
  id: string;
  name: string;
  logoUrl: string | null;
  document: string;
  email: string;
  phone: string;
  address: {
    zipCode: string;
    street: string;
    number: string;
    complement: string;
    neighborhood: string;
    city: string;
    state: string;
  };
  shippingDefaults: {
    packageWeightGrams: number;
    packageFormat: "box" | "roll" | "letter";
  };
};
```

## 8. Commit / deploy

- Commit: `aaf9a74` em `origin/main`
- Migration: `000052_shipments.up.sql` (tabelas `shipments` + `shipment_tracking_events`)
- Sem breaking change em endpoints existentes — só adição de campos opcionais
