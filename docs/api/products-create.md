# Cadastro de produtos — contrato HTTP

Doc de referência para o frontend implementar a tela "Novo produto".

> Status: **rota já existe e funciona**. Se a tela do front diz "não funciona", é provável diff de naming entre o payload enviado e o esperado abaixo. Compare campo a campo.

---

## 1. Produto simples (sem variantes)

### `POST /api/v1/stores/:storeId/products`

**Headers**
```
Authorization: Bearer <CLERK_JWT>
Content-Type: application/json
```

**Body**
```json
{
  "name": "Camiseta básica preta",
  "externalId": "",
  "externalSource": "manual",
  "keyword": "",
  "price": 4990,
  "imageUrl": "https://cdn.livecart.com/products/abc.png",
  "stock": 25,
  "shipping": {
    "weightGrams": 250,
    "heightCm": 5,
    "widthCm": 25,
    "lengthCm": 30,
    "sku": "CAM-BSC-PT",
    "packageFormat": "box",
    "insuranceValueCents": 4990
  }
}
```

| Campo | Tipo | Obrigatório | Notas |
|---|---|---|---|
| `name` | string (1–200) | ✅ | |
| `externalSource` | enum | ✅ | `manual` \| `tiny` \| `bling` \| `shopify`. Use `manual` para cadastro manual. |
| `externalId` | string | ❌ | Vazio para `manual`. Usado quando o produto vem de ERP. |
| `keyword` | string (4 chars) | ❌ | Auto-gerado se vazio. Quando informado deve ser único na loja. |
| `price` | int (cents) | ✅ | `4990` = R$ 49,90. |
| `imageUrl` | string | ❌ | URL pública. |
| `stock` | int ≥ 0 | ✅ | |
| `shipping.weightGrams` | int > 0 | ❌* | Obrigatório se algum dos 4 (peso/altura/largura/comprimento) for informado. |
| `shipping.heightCm/widthCm/lengthCm` | int > 0 | ❌* | All-or-nothing junto com weight. |
| `shipping.sku` | string ≤ 100 | ❌ | SKU "real" para envio. Diferente de keyword. |
| `shipping.packageFormat` | enum | ❌ | `box` (default) \| `roll` \| `letter`. |
| `shipping.insuranceValueCents` | int ≥ 0 | ❌ | Cai no preço se vazio. |

> \* "all-or-nothing" significa: ou os 4 campos `weightGrams/heightCm/widthCm/lengthCm` vêm preenchidos, ou nenhum vem. Backend retorna `400` se misturar.

**Resposta 201**
```json
{
  "data": {
    "id": "uuid",
    "name": "Camiseta básica preta",
    "keyword": "0001",
    "createdAt": "2026-04-25T12:00:00Z"
  }
}
```

**Erros comuns**
- `400` — body inválido, package format desconhecido, dimensões parciais
- `409` — keyword já em uso na loja
- `422` — validação de campo (faltou `name`, price negativo, etc.) — payload contém detalhes
- `404` — store não existe ou usuário não tem acesso

**curl**
```bash
curl -X POST 'https://api.livecart.com/api/v1/stores/STORE_UUID/products' \
  -H 'Authorization: Bearer CLERK_JWT' \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Camiseta básica preta",
    "externalSource": "manual",
    "price": 4990,
    "stock": 25,
    "shipping": { "packageFormat": "box" }
  }'
```

---

## 2. Produto com variantes (cor × tamanho, etc.)

### `POST /api/v1/stores/:storeId/product-groups`

Cria o agregador (grupo) + as opções/valores + N variantes numa única chamada. Cada variante vira um produto vendável (com keyword/preço/estoque/SKU próprios) ligado ao grupo.

**Body**
```json
{
  "name": "Camiseta básica",
  "description": "100% algodão",
  "options": [
    { "name": "Cor", "values": ["Preto", "Azul"] },
    { "name": "Tamanho", "values": ["P", "M", "G"] }
  ],
  "groupImages": [
    "https://cdn.livecart.com/products/group/abc-1.png",
    "https://cdn.livecart.com/products/group/abc-2.png"
  ],
  "variants": [
    {
      "optionValues": ["Preto", "P"],
      "price": 4990,
      "stock": 10,
      "sku": "CAM-PT-P",
      "keyword": "",
      "imageUrl": "https://cdn.livecart.com/products/var/preto-p.png",
      "images": ["https://cdn.livecart.com/products/var/preto-p-2.png"],
      "shipping": { "weightGrams": 250, "heightCm": 5, "widthCm": 25, "lengthCm": 30, "packageFormat": "box" }
    },
    {
      "optionValues": ["Preto", "M"],
      "price": 4990,
      "stock": 8,
      "sku": "CAM-PT-M"
    }
  ]
}
```

| Campo | Tipo | Obrigatório | Notas |
|---|---|---|---|
| `name` | string (1–200) | ✅ | Nome do produto-pai. |
| `description` | string | ❌ | Texto livre. |
| `options[].name` | string | ✅ | Ex: "Cor", "Tamanho". |
| `options[].values[]` | string[] | ✅ | Pelo menos 1 valor. Ordem é preservada. |
| `groupImages[]` | string[] | ❌ | Galeria do grupo (fotos genéricas/modelo). |
| `variants[]` | array | ✅ | Pelo menos 1 variante. |
| `variants[].optionValues` | string[] | ✅ | Valores na **mesma ordem** das `options`. Ex: `["Preto","P"]` se options = `[Cor, Tamanho]`. |
| `variants[].price` | int (cents) | ✅ | Preço por variante. |
| `variants[].stock` | int ≥ 0 | ✅ | |
| `variants[].sku` | string | ❌ | SKU de envio. |
| `variants[].keyword` | string (4) | ❌ | Auto-gerado se vazio. Único na loja. |
| `variants[].imageUrl` | string | ❌ | Imagem principal/thumbnail da variante. |
| `variants[].images[]` | string[] | ❌ | Galeria adicional da variante. |
| `variants[].shipping` | objeto | ❌ | Mesmo schema do produto simples. |

**Resposta 201**
```json
{
  "data": {
    "id": "uuid-do-grupo",
    "name": "Camiseta básica",
    "variants": [
      { "id": "uuid", "keyword": "0001", "optionValues": ["Preto","P"] },
      { "id": "uuid", "keyword": "0002", "optionValues": ["Preto","M"] }
    ],
    "createdAt": "2026-04-25T12:00:00Z"
  }
}
```

**Validações**
- A combinação de `optionValues` deve ser única dentro do grupo (não pode ter duas variantes "Preto+P")
- Cada `variants[].optionValues[i]` deve existir em `options[i].values`
- O comprimento de `optionValues` deve bater com o número de `options`

---

## 3. Listagem e detalhe

- `GET /api/v1/stores/:storeId/products` — retorna **variantes** (cada variante é uma row em `products`). Use `?groupId=...` para filtrar variantes de um grupo.
- `GET /api/v1/stores/:storeId/products/:id` — retorna a variante com `groupId`, `optionValues` (denormalizado), `images`.
- `GET /api/v1/stores/:storeId/product-groups` — lista grupos com `variantsCount`.
- `GET /api/v1/stores/:storeId/product-groups/:id` — detalhe completo do grupo + opções + valores + variantes + galeria.

A `ProductResponse` ganha:
```json
{
  "id": "uuid",
  "groupId": "uuid|null",
  "optionValues": [
    { "option": "Cor", "value": "Preto" },
    { "option": "Tamanho", "value": "P" }
  ],
  "images": ["url1", "url2"],
  ... // demais campos atuais (name, keyword, price, stock, imageUrl, shipping)
}
```

Produtos simples retornam `groupId: null` e `optionValues: []`. Nenhum campo existente foi removido — é tudo aditivo.
