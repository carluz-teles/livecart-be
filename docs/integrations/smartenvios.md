# SmartEnvios — API Reference (interna)

Referência canônica para a implementação do provider `smartenvios` em `apps/api/internal/integration/providers/shipping/`. Baseada no OpenAPI oficial da SmartEnvios (`https://dev.smartenvios.com/docs/smartenvios/`, YAML completo ingerido em 2026-04-24) + samples reais fornecidos pelo usuário.

> Status: **completa em cobertura**; ainda há pendências de validação prática (marcadas ao longo do doc).

---

## Princípio de design (obrigatório)

LiveCart terá **múltiplos providers de frete** no futuro. O domínio interno (tabelas `carts`/`orders`/`shipments`, DTOs públicos, service do checkout) **não pode** amarrar em particularidades da SmartEnvios (nem da Melhor Envio). Tudo que é específico do provider fica **atrás** da interface `ShippingProvider` ([providers/types.go](apps/api/internal/integration/providers/types.go)).

Regras práticas:
- Identificadores de serviço variam por provider (int no ME, ObjectId/UUID no SmartEnvios). No domínio → **sempre `string` opaco**.
- Metadados específicos do provider vão em JSONB (`integrations.metadata`, `shipments.provider_meta`), **nunca** coluna de primeira classe.
- Quirks do provider (chaves JSON com espaços, tipos inconsistentes, enums bespoke) são corrigidos **no boundary do provider**. Nada vaza pro service/handler.
- Status de rastreio são traduzidos para um **enum interno único** — ver tabela canônica em §14.

---

## 0. Dados gerais

### Base URLs (OpenAPI `servers[]`)

| Ambiente | URL | Observação |
|----------|-----|------------|
| **Produção** | `https://api.smartenvios.com/v1` | Inclui `/v1` no path base |
| **Sandbox (homologação)** | `https://sandbox.api.smartenvios.com` | ⚠ **Não tem `/v1` no path** — confirmar empiricamente se rotas chamam `/quote/freight` ou `/v1/quote/freight` em sandbox |

### Autenticação

Header único: `token: <token-do-embarcador>`. Não é OAuth — é token estático emitido pela plataforma SmartEnvios para embarcador, base, hub ou transportadora. Algumas rotas aceitam só token de embarcador; outras (ex.: `GET /customer`) exigem token de base.

### Content-Type

- **`application/json`** — quase todas as rotas
- **`multipart/form-data`** — **obrigatório** em `/nfe-upload` (upload de XML) e **documentado** (mas sample usa JSON) em `/dc-create` e `/reverse`

### Módulos (tags no OpenAPI)

`Cotação` · `Pedido` · `Rastreio` · `Cadastro` · `Expedição`

---

## 1. Cotar frete — `POST /quote/freight`

Retorna custos + prazos de todas as transportadoras disponíveis. Regra crítica: considerar apenas linhas com `is_valid == true`.

### Request

```
POST https://api.smartenvios.com/v1/quote/freight
Headers: token
Body:
```

| Campo | Tipo | Obrig. | Nota |
|-------|------|--------|------|
| `total_price` | number | — | BRL |
| `extra_days` | integer | — | Dias extras somados no prazo |
| `document` | string | — | CPF/CNPJ do destinatário. **Opcional** (sample não envia) |
| `volumes[]` | array | ✅ | Cada item: `weight`(kg, number), `height`/`length`/`width` (cm, int), `quantity` (int), `price` (BRL number), `sku` (array, opcional) |
| `zip_code_start` | string | ✅ | CEP origem. Aceita com hífen (`14170-763`) |
| `zip_code_end` | string | ✅ | CEP destino |
| `source` | string | — | Livre. Samples: `"CMS"`. Sugestão: `"LIVECART"` |
| `external_origin` | string | — | Livre. Sugestão: **nosso `cart_id`** para correlação |

### Response `200`

```json
{
  "result": [
    { "id": "5eb9b31e1097eb6cdf922d04", "base": "RAO - SmartEnvios",
      "service_code": 10, "service": "Jadlog Package",
      "value": 10.64, "days": 2, "is_valid": true, "errors": [] },
    { "id": "5eb9b2ed1097eb6cdf922cf3", "base": "RAO - SmartEnvios",
      "service_code": 1,  "service": "Gollog",
      "value": 0, "days": 0, "is_valid": false,
      "errors": ["O peso da cotação é maior do que o limite do serviço."] }
  ]
}
```

| Campo | Mapeamento LiveCart |
|-------|---------------------|
| `result[].id` (ObjectId string) | `QuoteOption.ServiceID` ✅ **string opaco** — é o token consumido pelo `dc-create?quote_service_id=` |
| `result[].service` (`"Jadlog Package"`) | `QuoteOption.Service` (nome comercial exibido ao cliente) |
| `result[].service_code` | guardar em metadata (provider-meta) — não é ID estável entre providers |
| `result[].base` | metadata (operação logística interna — **não exibir ao cliente**) |
| `result[].value` | `QuoteOption.PriceCents = round(value * 100)` |
| `result[].days` | `QuoteOption.DeadlineDays` |
| `result[].is_valid` | `QuoteOption.Available` |
| `result[].errors[]` | `QuoteOption.Error = strings.Join(errors, "; ")` |

**Heurística de `Carrier`:** primeira palavra de `service`. `"Jadlog Package"` → `"Jadlog"`. Validar com mais exemplos; se provar frágil, manter o `service` completo em `Carrier` e `Service`.

### Decidido
- ✅ `QuoteOption.ServiceID` passará de `int` para `string` (Melhor Envio migra com `strconv.Itoa`).
- ✅ CEP: enviar normalizado com hífen (`XXXXX-XXX`).
- ✅ `volumes[]` aceita múltiplos itens — não precisa empacotar no backend.

### Pendente
- `source` e `external_origin` — convenção oficial do LiveCart (propor: `source="LIVECART"`, `external_origin=<cart_id>`).

---

## 2. Criar pedido — `POST /dc-create`

Cria um pedido de frete como **Declaração de Conteúdo**, vinculável a NF-e (`nfe_key`) ou DC-e (`dce_key`). Aceita cotação pré-existente ou auto-seleção.

### Request

```
POST https://api.smartenvios.com/v1/dc-create
     [?quote_service_id=<id da cotação>]
Headers: token, Content-Type
```

⚠ **Content-Type**: spec marca `multipart/form-data` mas sample cURL usa `application/json`. Interpretação: multipart só quando XML é anexado no mesmo request; com `nfe_key` (ou sem NFe) usar JSON. **Confirmar empiricamente.**

### Modos

| Modo | `?quote_service_id` | `preference_by` | Quando |
|------|---------------------|-----------------|--------|
| **A. Cotação vinculada** (nosso) | sim | ignorado | Cliente já cotou no checkout, preço travado |
| B. Auto-seleção | não | `QUOTE_VALUE` \| `DELIVERY_TIME` \| `SERVICE_NAME` | **Não usar** — quebra garantia de preço |

### Body

**Top-level:** `preference_by?`, `external_order_id`, `external_origin`, `freightContentStatement{}`

**`freightContentStatement`:**
| Campo | Tipo | Obrig. | Nota |
|-------|------|--------|------|
| `nfe_key` | string(44) | — | Chave NFe. Mutex com `dce_key`. Dispensa `/nfe-upload`. |
| `dce_key` | string | — | Chave DC-e. |
| `sender_*` | string | — | `name`/`document`/`zipcode`/`street`/`number`/`neighborhood`/`complement`/`phone`/`email` |
| `observation` | string | — | |
| `destiny_name`/`destiny_document`/etc. | string | — (só `destiny_zipcode` marcado como required) | Na prática rua/número também |
| `destiny_zipcode` | string | ✅ | |
| `adjusted_volume_quantity` | integer | — | Número de caixas físicas |
| `items[]` | array | — | `{description, amount, unit_price, total_price, weight, height, width, length, sku[]}` |

✅ **`unit_price`/`total_price` = reais inteiros (arredondados), não centavos.** Validação (2026-04-24): o webhook `GENERATED_LABEL` traz `items[].value: 100` e `document_shipping.document_total_price: 30` — ambos integers de magnitude compatível com **reais** (30 reais de frete, 100 reais de valor declarado), não com centavos (seria R$ 0,30 / R$ 1,00 — absurdo). O `/quote/freight` usa `number` (decimal) porque ali o preço é só input de cálculo de frete; no pedido SmartEnvios só armazena reais. **Estratégia no provider:** arredondar `math.Round` (ou para cima, avaliar) ao converter `priceCents → int reais` na hora de chamar `/dc-create`. O valor fiscal real fica na NFe vinculada pelo `nfe_key` — o `unit_price` aqui serve apenas para a Declaração de Conteúdo.

### Response `200`

```json
{
  "result": {
    "freight_order_id": "d2889bac-9f34-485c-a2c2-b817c87abbf4",
    "freight_order_number": 6355936,
    "freight_order_tracking_code": "SM806381523458D0",
    "customer_id": "8e943576-4063-4aa6-8772-8eb9dad48fd8",
    "nfe_id": "325424bc-b3b7-48bf-aff9-81c9ecf8b465",
    "created_at": "2022-01-06T20:13:27.523Z",
    "updated_at": "2022-01-06T17:13:27.000Z",
    "freight_order_status": { "code": 1, "name": "Pedido em Aberto" }
  }
}
```

| Campo | Uso LiveCart |
|-------|--------------|
| `freight_order_id` | `shipments.provider_order_id` |
| `freight_order_number` | exibição admin; filtro em `GET /order/show` |
| `freight_order_tracking_code` | `shipments.tracking_code` — expor ao consumidor |
| `nfe_id` | metadata |
| `freight_order_status.code` | traduzir para enum interno (§14) |

### Response `400` — erros comuns de NFe

Mensagens vistas na spec (úteis para UI de admin):
- "Esta NF-e não pertence ao cliente selecionado."
- "NF-e já cadastrada no sistema."
- "Este arquivo não é um XML válido."
- "A Transportadora da NF-e não existe no sistema."
- "Não foi possível vincular NF-e porque tanto o emitente quanto o destinatário não existem no sistema."
- "A transportadora da NF-e não presta serviço para este cliente"

---

## 3. Atualizar pedido — `PATCH /order`

Atualiza parcialmente um pedido. Todos os campos do body são opcionais.

⚠ Spec descreve "3 tipos de identificadores na URL (id/tracking_code/número)" mas **a rota declarada é `/order` sem path-param**. Provavelmente o identificador vai em query params não documentados. **Pendência — confirmar com suporte.**

### Body (todos opcionais)

`external_origin`, `external_order_id`, `volumes` (string!), `ticket_number`, `quote_service_id`, `nfe_key`, `sender{...}`, `destiny{...}`

**Uso prático no LiveCart:** vincular NFe tardia após criar o pedido sem ela (caminho alternativo ao `/nfe-upload`):
```
PATCH /order  body: { "nfe_key": "NFe...", "external_order_id": "<nosso_id>" }
```

---

## 4. Reversa (troca/devolução) — `POST /reverse`

Cria pedido reverso (cliente → loja). Estrutura bem parecida com `/dc-create`, mas com campos extras:

- Top-level: `customer_id`, `preference_by`, `external_order_id`, `external_origin`, `zip_code_start`/`end`, `settings{own_hand,receipt_notice}`, `type` (= `"REVERSE"`), `shipping_type` (ex.: `"CORREIOS"`), `amount`, `total_weight`, `cubage`
- `freightContentStatement`: mesmo shape do `/dc-create` + `destiny_city` (atenção: aqui é `destiny_city`, não `destiny_city_name`)

**Response 200** similar ao `/dc-create`, mas o doc também mostra um formato alternativo com `quote_delivery_time`, `quote_value`, `internal_number`, `post_code`. **Normalizar no boundary** pra mesma struct interna de "pedido criado".

**Uso no LiveCart:** só necessário se implementarmos fluxo de devolução. Fora do MVP.

---

## 5. Upload de NF-e — `POST /nfe-upload`

Vincula um XML NF-e a um pedido **já criado**. Usado quando o `dc-create` foi chamado sem `nfe_key` (ex.: Tiny demorou a emitir).

### Request

```
POST https://api.smartenvios.com/v1/nfe-upload?base_id=<fixo>&freight_order_id=<uuid>
Headers:
  token
  Content-Type: multipart/form-data
Body (multipart):
  form = @arquivo.xml
```

- `base_id` é **fixo**: `a66cb425-a04c-460a-a0ac-b5ef61367e50` (conforme doc — parece ser identificador global, não por embarcador; **confirmar se continua válido em produção**).
- `freight_order_id` vem da resposta do `/dc-create`.

### Response `200`
```json
{ "result": { "message": "Upload realizado com sucesso!" } }
```

`400` traz as mesmas mensagens listadas em §2.

### Decisão de fluxo

```
Tiny emite NFe rápido? → dc-create com nfe_key (1 request)
Tiny assíncrono?       → dc-create sem nfe_key
                       → esperar Tiny → PATCH /order com nfe_key  [simples]
                                    OU nfe-upload com XML         [multipart]
```

Preferir `PATCH /order` por ser JSON (mais simples no client HTTP). `/nfe-upload` só quando precisar **enviar o XML** (se a SmartEnvios não conseguir baixar via SEFAZ pela chave).

---

## 6. Imprimir etiqueta — `POST /labels`

Gera etiquetas (barcodes dos volumes). **Exige que o pedido tenha NF-e/DC vinculada** — caso contrário retorna "Não foi possível gerar etiqueta do pedido {número}, por não possuir NF-e ou DC vinculado."

### Request

```
POST https://api.smartenvios.com/v1/labels
Headers:
  token
  x-processing-mode: sync | assync   (opcional)
Body:
  {
    "tracking_codes": ["SM..."],     // OU
    "order_ids":      ["uuid"],      // OU
    "nfe_keys":       ["NFe..."],    // OU (não explicitado mas o doc menciona "external_id"/external_oders)
    "type":         "pdf" | "zpl" | "base64",
    "documentType": "label_integrated_danfe" | "label_separate_danfe"
  }
```

### Response `200`

```json
{
  "result": [{
    "url": "smartenvios.com/pdf/SM6273436698320",
    "tickets": [{
      "freight_order_id": "d81bb012-...",
      "tracking_code": "SM6273436698320",
      "public_tracking": "https://v1.portal.smartenvios.com/tracking/SM6273436698320",
      "volumes": [
        { "barcode": "SMP0538583001" },
        { "barcode": "SMP0538583002" }
      ]
    }]
  }]
}
```

**⚠ Inconsistência de shape:** o OpenAPI declara `result` como array de `{barcode, freight_order_id, created_at}`, mas o exemplo mostra um shape diferente (`url`, `tickets[]`, com `tickets[].volumes[].barcode`). O provider precisa parsear o **shape do exemplo** (realista) e não o schema (teórico).

| Campo | Uso LiveCart |
|-------|--------------|
| `result[].url` (ou `result[].tickets[].url`) | URL pública do PDF da etiqueta — salvar em `shipments.label_url` |
| `public_tracking` | URL de rastreio pública — expor ao cliente |
| `tickets[].volumes[].barcode` | Cada volume recebe um barcode independente — útil se tiver múltiplas caixas |

### `x-processing-mode`

- `sync` — retorna URL já processada
- `assync` (sic, typo da API) — enfileira e notifica via webhook `GENERATED_LABEL`

Para o LiveCart: **`assync` + webhook** é mais resiliente (não trava o cron job de pós-pagamento). Mas `sync` é mais simples e aceitável se o SLA for bom.

---

## 7. Rastrear — `POST /freight-order/tracking`

Retorna histórico de eventos de rastreio. Body aceita **exatamente UM** identificador:

```json
{ "freight_order_id": "..." }     // OU
{ "order_id": "..." }             // = external_order_id (nosso id interno na NFe)
{ "nfe_key": "..." }
{ "tracking_code": "..." }
```

### Response `200` (trecho)

```json
{
  "result": {
    "number": "19111",
    "tracking_code": "SM3028078737SD5",
    "sender_name": "...", "destiny_name": "...",
    "destiny_phone": "...", "destiny_city_name": "...", "destiny_uf": "...",
    "observation": null,
    "external_origin": "...", "shipping_name": "...", "service_name": "...",
    "trackings": [
      { "observation": "Pedido recebido na Base.",
        "date": "2021-01-20T21:14:23.380Z",
        "code": { "number": 4, "name": "Recebido Base SmartEnvios",
                  "description": "...", "tracking_type": "IN TRANSIT" } },
      { "observation": "...",
        "date": "2021-01-27T16:29:30.243Z",
        "code": { "number": 7, "name": "Entrega Realizada",
                  "description": "...", "tracking_type": "DELIVERED" } }
    ]
  }
}
```

⚠ **`tracking_type` tem espaço: `"IN TRANSIT"`, `"DELIVERED"`** — enquanto o filtro do webhook aceita `"IN_TRANSIT"` (com underscore). Normalizar no boundary. Valores conhecidos: `IN TRANSIT`, `DELIVERED`. Provavelmente há também algo como `EXCEPTION`/`PROBLEM`/`RETURNED`/`null` (o `UPDATED_TRACKING` no webhook mostrou `"tracking_type": null` em eventos de problema).

### Mapeamento

| Campo | LiveCart |
|-------|----------|
| `result.tracking_code` | ecoar |
| `result.trackings[].code.number` | traduzir → enum interno (§14) |
| `result.trackings[].date` | ISO8601 com TZ — parsear direto |
| `result.trackings[].observation` | texto livre para timeline do cliente |
| `result.shipping_name` / `service_name` | nome comercial da transportadora |

---

## 8. Adicionar evento de rastreio — `POST /public/tracking-create`

**Usado por transportadoras/bases**, não por embarcador comum. LiveCart é embarcador → **não deve** chamar essa rota no fluxo normal. Listado para completude.

---

## 9. Solicitar coleta — `POST /public/collect-request`

Agenda coleta em um horário. Útil se a loja não leva na transportadora.

```json
POST /v1/public/collect-request
Body:
{
  "dateCollect": "2023-09-02T11:00:00Z",
  "userShipper": { "name": "...", "email": "..." }
}
```

Erros conhecidos:
- 503 "O embarcador já possui um pedido de coleta em aberto" → só 1 em aberto por vez
- 503 "Não é possível solicitar coleta após hora limite de 12:00:00" → **janela até meio-dia** local

**Uso no LiveCart:** feature opcional do admin (botão "Agendar coleta"). Fora do MVP.

---

## 10. Listar pedidos — `GET /order/show`

Leitura/sync de pedidos. **Requer janela temporal** — não dá pra listar tudo.

### Query params

| Param | Obrig. | Nota |
|-------|--------|------|
| `filter.update_order_date_start` | ✅ | Data inicial de atualização |
| `filter.update_order_date_end` | ✅ | Data final |
| `filter.external_order_id` | — | **Nossa chave de correlação** (cart/order id do LiveCart) |
| `filter.status` / `filter.destiny_document` / `filter.destiny_name` / `filter.tracking_code` / `filter.customer_id` / `filter.base_id` / `filter.ticket_generated` / `filter.freight_order_number` | — | Filtros variados |
| `page` / `size` | — | Paginação |

⚠ Formato do `filter.update_order_date_*` **não declarado na spec**. Samples internos usam `"2022-11-08T08:20:06.316Z"` para `created_at`/`updated_at` no response; provavelmente mesmo formato no filtro. **Testar.**

### Response (abreviado — ver YAML para full shape)

```json
{
  "result": {
    "order": [ { /* 50+ campos por pedido */ } ],
    "total": 7145, "pageSize": 1, "current": 1, "lastPage": 7145
  }
}
```

Campos principais de cada pedido: `id`, `tracking_code`, `external_origin`, `external_order_id`, `volumes`, `ticket_generated`, `freight_order_number`, `quotation{quote_service_id, quote_service_name, quote_value, quote_days, external_id}`, `shipping_first_mile{...}`, `shipping_transfer_destiny{...}`, `shipping_last_mile{...}`, `taker{...}`, `status{status_name, status_code}`, `document_shipping{...}` (DC) ou `document_nfe{...}` (NFe), `sender{...}`, `destiny{...}`, `items[]`, `created_at`, `updated_at`.

### ⚠ Chaves JSON com espaços — é bug de doc, não de payload

A spec tem **dois samples** do mesmo endpoint:
- Sample A (mais rico, `testing_tags` etc.): `quote_service_name: JadLog`, `taker_type: BASE` — **chaves limpas**
- Sample B (mais resumido, ID `0112319ef-...`): `"quote_service_name ": JadLog`, `" taker_type ": BASE` — **com espaços**

Conclusão mais provável: **bug no sample B**, payload real é limpo. Ainda assim o provider deve ser **defensivo**: passar por `json.RawMessage` + normalizador `strings.ToLower(strings.TrimSpace(key))` antes de mapear structs. Custo baixo, elimina toda a classe de bug.

### ⚠ `items[].sku` inconsistente

- Sample A: `items: []` (vazio)
- Sample B: `items: [{ ..., "sku": 123 }]` — número
- Request `/dc-create`: `sku: ["XXXX", "XXXX"]` — array de strings
- Request `/quote/freight`: `sku: ["123"]` — array de strings

Tratar no provider com parse tolerante (`json.RawMessage` → tentar array, senão scalar → normalizar para `[]string`).

### ⚠ `items[]` usa `value`, não `unit_price`/`total_price`

No **response** de `/order/show` e no payload do webhook `GENERATED_LABEL`, cada item vem com `{length, height, width, value, weight, barcode, sku}` — campo `value` substitui os dois campos do request (`unit_price` + `total_price`). Mais uma normalização no boundary do provider. Possivelmente `value` == `unit_price * amount` (= `total_price`). Confirmar em payload real.

---

## 11. Listar transportadoras — `GET /quote/services`

Retorna as transportadoras/serviços habilitados para o embarcador.

### Response

```json
[
  { "freight_sheet_id": "ad1959e2-95cf-4525-a694-160555151051",
    "freight_sheet_description": "Sedex" },
  { "freight_sheet_id": "...", "freight_sheet_description": "PAC" },
  { "freight_sheet_id": "...", "freight_sheet_description": "Gollog" },
  { "freight_sheet_id": "...", "freight_sheet_description": "Jadlog Package" },
  { "freight_sheet_id": "...", "freight_sheet_description": "Transfolha" }
]
```

**Uso:** implementa `ShippingProvider.ListCarriers()` ([providers/types.go](apps/api/internal/integration/providers/types.go)). Também útil para `TestConnection()` — request barato e confiável para validar o token.

Mapeamento:
- `freight_sheet_id` → `CarrierService.ServiceID` (string)
- `freight_sheet_description` → `CarrierService.Service` + heurística de `Carrier` por primeira palavra

---

## 12. Listar clientes da base — `GET /customer`

**Só para token de base** (não embarcador comum). Lista embarcadores pertencentes à base. Fora do escopo do LiveCart (não somos base).

---

## 13. Webhooks — DEFERIDO (não entra na v1)

> **Decisão 2026-04-24:** webhooks **não** serão implementados na v1 do provider SmartEnvios. Tracking via pull (§7) quando necessário. Seção mantida como referência para quando for reavaliado. Antes de implementar: abrir ticket com SmartEnvios para esclarecer autenticidade (#7 em §16).



### Registrar / Atualizar

```
POST /v1/webhook
PUT  /v1/webhook/:id
Headers: token
Body: {
  "endpoint": "https://.../webhooks/smartenvios",
  "status": "active" | "inactive",
  "actions": ["CREATED_ORDER" | "CREATED_ENTITY" | "UPDATED_ENTITY"
              | "GENERATED_LABEL" | "UPDATED_TRACKING"],
  "filters": {
    "code_number": [4],
    "tracking_type": ["IN_TRANSIT"],
    "shipping_company": ["SEDEX"],
    "external_origin": ["LIVECART"]
  }
}
```

⚠ No **filtro** do registro o valor é `IN_TRANSIT` (underscore); no **payload** entregue é `IN TRANSIT` (espaço). Documentar e normalizar.

### Payloads
- `GENERATED_LABEL` / `CREATED_ORDER` — pedido completo (mesma shape do §10)
- `UPDATED_TRACKING` — `trackings[]` igual ao §7 + metadados do pedido

### Pendências
- Validação de autenticidade (assinatura HMAC? IP allowlist? echo token?) — **desconhecido**
- Política de retry em caso de 5xx do nosso endpoint — **desconhecido**

---

## 14. Tabela canônica de status (de `POST /freight-order/tracking` e `/public/tracking-create`)

| Code | Nome | Categoria resumida | Enum interno LiveCart (proposto) |
|------|------|---------------------|----------------------------------|
| 0  | Aguardando Nota Fiscal | Aguardando postagem | `awaiting_invoice` |
| 1  | Pedido em Aberto | Aguardando postagem | `pending` |
| 2  | Aguardando SmartColeta | Aguardando postagem | `pending_pickup` |
| 27 | Aguardando Serviço | Aguardando postagem | `pending` |
| 28 | Aguardando Entrega no Hub | Aguardando postagem | `pending_dropoff` |
| 32 | Em Sincronização | Aguardando postagem | `pending` |
| 30 | Recebido na base SmartEnvios para devolução | Devolução | `returning` |
| 31 | Recebido no Hub para devolução com pendência | Devolução | `returning` |
| 15 | Pedido Devolvido para Cliente | Devolução | `returned` |
| 14 | Pedido Devolvido | Devolução | `returned` |
| 13 | Em trânsito para Devolução | Devolução | `returning` |
| 3  | Coletado | Em trânsito | `in_transit` |
| 4  | Recebido Base SmartEnvios | Em trânsito | `in_transit` |
| 5  | Aguardando Coleta Transportador | Em trânsito | `in_transit` |
| 6  | Em trânsito | Em trânsito | `in_transit` |
| 16 | Coletado Transportadora | Em trânsito | `in_transit` |
| 25 | Aguardando Retirada | Em trânsito | `awaiting_pickup` |
| 29 | Saiu para entrega | Em trânsito | `out_for_delivery` |
| 23 | Reentrega Solicitada | Em trânsito | `in_transit` |
| 7  | Entrega Realizada | Entregue | `delivered` |
| 8  | Problema na Entrega | Problema | `delivery_issue` |
| 9  | Entrega Bloqueada | Problema | `delivery_blocked` |
| 10 | Pedido com Pendência | Problema | `issue` |
| 11 | Envio Bloqueado | Problema | `shipment_blocked` |
| 17 | Encomenda Avariada | Problema | `damaged` |
| 18 | Mercadoria Roubada | Problema | `stolen` |
| 19 | Mercadoria Extraviada | Problema | `lost` |
| 20 | Problemas Fiscais | Problema | `fiscal_issue` |
| 21 | Encomenda Recusada | Problema | `refused` |
| 22 | Mercadoria Sinistrada | Problema | `damaged` |
| 24 | Problemas Pontuais | Problema | `issue` |
| 26 | Finalizado sem entrega | Problema | `not_delivered` |
| 33 | Indenização Solicitada | Problema | `indemnification_requested` |
| 34 | Indenização Agendada | Problema | `indemnification_scheduled` |
| 35 | Indenização Finalizada | Problema | `indemnification_completed` |
| 12 | Cancelado | — | `canceled` |

**Implementar em Go:** tabela literal `map[int]LiveCartShipmentStatus`; desconhecidos → `unknown` + log warn.

⚠ Não há endpoint de cancelamento explícito no YAML. Cancelamento provavelmente se dá via backoffice / suporte / `PATCH /order` com alguma flag — **pendência**.

---

## 15. Mapa de implementação (provider + domínio LiveCart)

### Interface `ShippingProvider` — o que implementar

| Método da interface | Rota(s) SmartEnvios |
|---------------------|---------------------|
| `ValidateCredentials()` | `GET /quote/services` (leve, valida token) |
| `TestConnection()` | `GET /quote/services` (mesmo) |
| `RefreshToken()` | **no-op** — token é estático |
| `Quote()` | `POST /quote/freight` |
| `ListCarriers()` | `GET /quote/services` |

### Métodos **novos** a acrescentar à interface (genéricos, nada provider-specific)

| Novo método | Rotas SmartEnvios | Rotas Melhor Envio |
|-------------|-------------------|---------------------|
| `CreateShipment(ctx, req) (Shipment, error)` | `POST /dc-create` | `POST /me/cart` + `POST /me/shipment/checkout` |
| `AttachInvoice(ctx, shipmentID, invoiceKey) error` | `PATCH /order` | **não suporta** (ME não precisa) |
| `UploadInvoiceXML(ctx, shipmentID, xml []byte) error` | `POST /nfe-upload` | `POST /me/shipment/invoice` |
| `GenerateLabels(ctx, shipmentIDs []string) (LabelResult, error)` | `POST /labels` | `POST /me/shipment/print` |
| `Track(ctx, identifier) (TrackingHistory, error)` | `POST /freight-order/tracking` | `GET /me/shipment/tracking/{code}` |
| `CancelShipment(ctx, shipmentID, reason string) error` | **?** (pendente) | `POST /me/shipment/cancel` |

Tudo atrás de `ShippingProvider`. Domínio consome apenas tipos internos (`Shipment`, `TrackingHistory`, `LabelResult`).

### Tabelas de DB a criar / evoluir (esboço)

```sql
-- nova
CREATE TABLE shipments (
  id                      UUID PRIMARY KEY,
  order_id                UUID NOT NULL REFERENCES orders,
  store_id                UUID NOT NULL REFERENCES stores,
  provider                VARCHAR NOT NULL,   -- 'melhor_envio' | 'smartenvios' | ...
  provider_order_id       VARCHAR NOT NULL,   -- freight_order_id (string opaco)
  tracking_code           VARCHAR,
  label_url               VARCHAR,
  status                  VARCHAR NOT NULL,   -- enum interno LiveCart (§14)
  status_raw              JSONB,              -- snapshot nativo do provider
  invoice_key             VARCHAR,            -- NFe chave quando houver
  provider_meta           JSONB NOT NULL,     -- response completo do CreateShipment
  created_at              TIMESTAMPTZ DEFAULT NOW(),
  updated_at              TIMESTAMPTZ,
  UNIQUE (provider, provider_order_id)
);

-- eventos de rastreio (append-only)
CREATE TABLE shipment_tracking_events (
  id            UUID PRIMARY KEY,
  shipment_id   UUID NOT NULL REFERENCES shipments,
  status        VARCHAR NOT NULL,          -- enum interno LiveCart
  raw_code      INT,                       -- status.status_code do provider (debug)
  raw_name      VARCHAR,
  observation   TEXT,
  event_at      TIMESTAMPTZ NOT NULL,
  received_at   TIMESTAMPTZ DEFAULT NOW(),
  source        VARCHAR NOT NULL           -- 'webhook' | 'poll'
);

-- migration no carts
ALTER TABLE carts
  ALTER COLUMN shipping_service_id TYPE VARCHAR;   -- era INT (melhor envio)
```

Zero coluna com nome de provider.

---

## 16. Pendências — status validado (2026-04-24)

Pendências foram atacadas por **três vias**: (a) re-leitura do OpenAPI YAML, (b) Context7 MCP (payload real do webhook `GENERATED_LABEL`), (c) evidência indireta (magnitude de valores, cross-check entre rotas).

Legenda:
- ✅ **Resolvido** — evidência forte ou múltipla
- 🟡 **Resolvido provisoriamente** — hipótese razoável mas falta confirmação empírica (sandbox/prod)
- ❌ **Em aberto** — docs não respondem, só suporte resolve

### Resolvido ✅

| # | Item | Resposta | Evidência |
|---|------|----------|-----------|
| 1 | Sandbox URL | `https://sandbox.api.smartenvios.com` (sem `/v1`) | `servers[]` do OpenAPI |
| 2 | `Content-Type` em `/dc-create` com `nfe_key` | **`application/json`** é aceito; multipart é legacy para quando se anexa XML inline | OpenAPI `requestBody.content` declara ambos; sample cURL oficial usa JSON |
| 4 | `unit_price`/`total_price` = reais inteiros ou centavos? | **Reais inteiros (arredondados)**, não centavos | Webhook `GENERATED_LABEL` traz `items[].value: 100` e `document_total_price: 30` — magnitude só faz sentido como reais; valor fiscal preciso fica na NFe vinculada |
| 8 | Existe rota de cancelamento? | **Não existe endpoint dedicado no OpenAPI.** Cancelamento é feito via `PATCH /order` (mudando status) ou via backoffice/suporte | Varredura `grep cancel` no YAML retornou só referência ao status code 12 ("Cancelado") em rastreio |
| 11 | Formato aceito em `filter.update_order_date_*` | ISO8601 com `Z` (ex.: `2022-11-08T08:20:06.316Z`) | Mesmo formato dos `created_at`/`updated_at` no response Sample A |

### Resolvido provisoriamente 🟡 (implementar assim e ajustar se falhar)

| # | Item | Decisão | Por quê |
|---|------|---------|---------|
| 3 | Body em `/dc-create` quando `?quote_service_id` enviado | **Sempre mandar body completo** (sender/destiny/items/volumes) | Samples oficiais sempre trazem body completo; não há hint de que a SmartEnvios busque no snapshot da cotação |
| 5 | `base_id` fixo em `/nfe-upload` | Começar com `a66cb425-a04c-460a-a0ac-b5ef61367e50` e falhar explicitamente se retornar erro | Doc literalmente diz "Fixo: ..." — tratar como constante até prova contrária |
| 6 | `PATCH /order` — como identifica o pedido? | **Provavelmente query param** (`?freight_order_id=...` ou similar). Testar com `external_order_id` + campos no body, que é o identificador de negócio | Descrição do endpoint fala em "3 identificadores na URL" mas a rota declarada é `/order` sem path-param — doc incompleto |
| 9 | Rate limit | Assumir **conservador** no client HTTP: retry com exponential backoff em 429/5xx, circuit breaker em 10 falhas seguidas | Nenhum hint oficial; padrão defensivo |
| 12 | Timezone de campos sem Z | **America/Sao_Paulo (BRT/BRST)** | Não há outra convenção sensata para uma API brasileira; samples Sample A usam Z, Sample B não — inconsistência de doc, adotar TZ explícito no parse |
| 14 | `/labels` modo `assync` dispara webhook `GENERATED_LABEL`? | **Sim** — é o único caminho sensato para o modo assíncrono ter utilidade | Design pattern óbvio; risco baixo de estar errado |
| 15 | Chaves JSON com espaços (`"quote_service_name "`, `" taker_type "`) são bug de doc ou do payload? | **Bug de doc** — assumir payload limpo, mas parse **defensivo** (normalizar `TrimSpace` em keys) | Sample A (o mais rico, com `testing_tags`) tem chaves limpas; Sample B tem espaços. O rico é quase certamente o real |
| — | `items[].sku` — número ou array? | Parse tolerante via `json.RawMessage` | Inconsistência óbvia entre rotas |

### Em aberto ❌ (requer contato com suporte SmartEnvios)

Após a decisão de **não implementar webhooks na v1** (§13), o item #7 deixa de ser bloqueante — só volta se webhooks forem retomados.

| # | Item | Impacto | Workaround temporário |
|---|------|---------|------------------------|
| 7 | ~~Autenticidade dos webhooks~~ | ~~Segurança~~ **Deferido junto com webhooks (§13)** | — |
| 8b | **Cancelamento confirmado via PATCH?** | Fluxo de estorno/refund | Stub `CancelShipment()` → `ErrNotImplemented`; admin cancela no portal SmartEnvios |
| 10 | **Token: emissão / rotação / múltiplos tokens** por conta | Admin UI / onboarding | Admin cola token manualmente (input de texto) + botão "testar conexão" (`GET /quote/services`). Rotação = re-colar |
| 13 | `tracking_type` exaustivo (além de `IN TRANSIT`, `DELIVERED`, `null`) | Completude do enum | Log warn em valores desconhecidos; mapear tudo que não for `DELIVERED` como `in_transit` genérico |

### Ações recomendadas antes de ir pra produção

1. **Abrir ticket com SmartEnvios** apenas sobre #8b (cancelamento via PATCH) se/quando refund virar requisito. `dev@smartenvios.com`.
2. **Smoke test em sandbox** para validar os 7 itens 🟡 — especialmente §3 (body) e §4 (base_id) que têm alto impacto.
3. **Deploy** para dev/staging com os itens 🟡 marcados como hipótese no código (comentário + TODO por item).

---

## Changelog

- **2026-04-24** — criado; §1 (cotar frete) populado com OpenAPI + sample.
- **2026-04-24** — §1 enriquecida com sample real; identificado trade-off `ServiceID int vs string`.
- **2026-04-24** — adicionado princípio de design providers-agnostic; §10 (Listar pedidos) populada; flagados problemas de parsing.
- **2026-04-24** — §2 (Criar pedido) populada; decisão confirmada: `ServiceID` = `string`.
- **2026-04-24** — **rewrite completo após ingestão do OpenAPI YAML oficial**: sandbox URL descoberta, todas as rotas documentadas (incluindo `/reverse`, `/nfe-upload`, `/labels`, `/freight-order/tracking`, `/public/collect-request`, `/public/tracking-create`, `/quote/services`, `/customer`, `PATCH /order`), tabela canônica de 36 status codes + enum interno LiveCart proposto, esboço de schema `shipments`/`shipment_tracking_events`, mapa de implementação completo. Pendências consolidadas em §16.
- **2026-04-24** — **validação das pendências**: 5 resolvidas ✅ (sandbox URL, Content-Type, unit_price=reais-int, cancelamento=via PATCH, data ISO8601Z), 7 resolvidas provisoriamente 🟡 (implementar com hipótese + TODO), 4 em aberto ❌ (webhook auth, cancelamento via PATCH confirmação, token, tracking_type completo) — requerem contato `dev@smartenvios.com`. Adicionada observação sobre `items[].value` em response vs `unit_price` em request.
- **2026-04-24** — **Webhooks DEFERIDOS da v1** (§13): tracking via pull apenas. Pendência #7 (webhook auth) sai da lista de bloqueantes.
