# Backend de Produtos — Variantes + Cadastro Manual (briefing front)

> Data: 2026-04-25 · Branch: `main` · API base: `http://localhost:3001` (dev) / `https://api.livecart.com` (prod)
> 
> Este doc explica **tudo** que está disponível no backend para a área de Produtos:
> cadastro manual (simples e com variantes), importação Tiny com variações, e os endpoints novos.
> 
> Existe também um doc de campo-a-campo só do POST `/products` em [products-create.md](products-create.md).

---

## 1. Conceito

| Termo | O que é |
|---|---|
| **Produto simples** | 1 SKU vendável. Sem variações. Linha em `products` com `groupId = null`. |
| **Variante** | 1 SKU vendável que pertence a um grupo. Cada variante é uma row em `products` com `groupId` apontando para o grupo. **Tem keyword/preço/estoque/SKU/imagem próprios**. |
| **Grupo (`product_group`)** | Agregador de catálogo (ex.: "Camiseta Básica") que define as **opções** (ex.: Cor, Tamanho) e seus **valores** (Preto/Azul, P/M/G). Não é vendável diretamente — quem é vendável são as variantes embaixo dele. |
| **Opção** | Dimensão de variação (ex.: "Cor"). |
| **Valor de opção** | Um valor concreto da opção (ex.: "Preto"). |

**Decisão importante**: cada variante é um produto vendável com **keyword única na loja**. Ex.: "Camiseta Preta P" tem keyword `1000`, "Camiseta Preta M" tem keyword `1001`. Isso significa que **carrinho, busca por keyword no live, reservas de estoque, pedidos do Tiny** continuam funcionando exatamente como antes — ninguém precisa mudar nada nessas áreas. O grupo é puramente um agregador para a UI de catálogo.

---

## 2. Endpoints disponíveis

Todos os endpoints abaixo:
- Headers: `Authorization: Bearer <CLERK_JWT>` + `Content-Type: application/json`
- Path prefix: `/api/v1/stores/:storeId`

### Produto (variante ou simples)

| Método | Path | O que faz |
|---|---|---|
| `GET` | `/products` | Lista produtos (variantes + simples). Suporta filtros e paginação (ver §4). |
| `GET` | `/products/stats` | Stats agregadas da loja (total, ativos, low stock, valor de estoque). |
| `POST` | `/products` | Cria 1 produto. Para simples: omitir `groupId`. Para variante avulsa: passar `groupId`. |
| `GET` | `/products/:id` | Detalhe de uma variante/produto, **incluindo `optionValues` e `images`**. |
| `PUT` | `/products/:id` | Atualiza name/price/stock/imageUrl/active/shipping. |
| `DELETE` | `/products/:id` | Deleta. |
| `POST` | `/products/:id/images` | Adiciona 1 imagem à galeria da variante. |
| `DELETE` | `/products/:id/images/:imageId` | Remove imagem da galeria. |

### Grupo de produto (agregador com variantes)

| Método | Path | O que faz |
|---|---|---|
| `POST` | `/product-groups` | **Cria grupo + opções + valores + N variantes em uma chamada atômica** (ver §3). |
| `GET` | `/product-groups` | Lista grupos da loja com `variantsCount`. |
| `GET` | `/product-groups/:id` | Detalhe completo (opções, valores, variantes, galeria). |
| `PUT` | `/product-groups/:id` | Atualiza nome/descrição. |
| `DELETE` | `/product-groups/:id` | Apaga o grupo (variantes ficam órfãs com `groupId=null`, **não são deletadas**). |
| `POST` | `/product-groups/:id/images` | Adiciona imagem genérica do grupo (foto de modelo, tabela de medidas etc.). |
| `DELETE` | `/product-groups/:id/images/:imageId` | Remove imagem do grupo. |

---

## 3. Fluxos do cadastro manual

A tela "Novo produto" precisa de **um seletor logo de cara**: **Produto simples** ou **Produto com variantes** (cor/tamanho/etc.). Os fluxos são diferentes:

### 3.1 Produto simples → `POST /products`

```http
POST /api/v1/stores/STORE_UUID/products
Authorization: Bearer JWT
Content-Type: application/json

{
  "name": "Caneca branca 300ml",
  "externalSource": "manual",
  "price": 2990,
  "stock": 50,
  "imageUrl": "https://cdn.livecart.com/p/caneca.png",
  "shipping": {
    "weightGrams": 350,
    "heightCm": 10,
    "widthCm": 9,
    "lengthCm": 9,
    "packageFormat": "box"
  }
}
```

Campos completos em [products-create.md](products-create.md). Pontos críticos:

- **`price` em CENTAVOS** (`2990` = R$ 29,90). Se mandar como decimal o backend vai aceitar `0`/zerar.
- **`externalSource: "manual"`** é obrigatório para produtos cadastrados manualmente. Se vier vazio cai no erro `400 invalid external source`.
- **`shipping.*` é all-or-nothing**: ou manda os 4 (`weightGrams`, `heightCm`, `widthCm`, `lengthCm`) ou nenhum. Misturar dá `400`.
- **`keyword` é opcional** — se vazia, backend gera (`0001`, `0002`…). Se passar, deve ser 4 dígitos numéricos entre `1000-9999`.

**Resposta 201:**
```json
{
  "data": {
    "id": "uuid",
    "name": "Caneca branca 300ml",
    "keyword": "0042",
    "createdAt": "2026-04-25T12:00:00Z"
  }
}
```

### 3.2 Produto com variantes → `POST /product-groups`

Esse é o endpoint novo. Cria tudo em **uma única transação**: grupo + opções + valores + N variantes + galeria. Se qualquer parte falhar, nada é persistido.

```http
POST /api/v1/stores/STORE_UUID/product-groups
Authorization: Bearer JWT
Content-Type: application/json

{
  "name": "Camiseta Básica",
  "description": "100% algodão pima",
  "options": [
    { "name": "Cor", "values": ["Preto", "Azul Marinho"] },
    { "name": "Tamanho", "values": ["P", "M", "G"] }
  ],
  "groupImages": [
    "https://cdn.livecart.com/p/camiseta-modelo-1.jpg",
    "https://cdn.livecart.com/p/camiseta-tabela-medidas.png"
  ],
  "variants": [
    {
      "optionValues": ["Preto", "P"],
      "price": 4990,
      "stock": 10,
      "sku": "CAM-PT-P",
      "imageUrl": "https://cdn.livecart.com/p/cam-preto-p.jpg",
      "images": ["https://cdn.livecart.com/p/cam-preto-p-2.jpg"],
      "shipping": { "weightGrams": 250, "heightCm": 5, "widthCm": 25, "lengthCm": 30, "packageFormat": "box" }
    },
    {
      "optionValues": ["Preto", "M"],
      "price": 4990,
      "stock": 8,
      "sku": "CAM-PT-M"
    },
    {
      "optionValues": ["Azul Marinho", "P"],
      "price": 4990,
      "stock": 5,
      "sku": "CAM-AZ-P"
    }
  ]
}
```

**Regras**:
- `options[].name` deve ser único dentro do grupo (não pode ter duas opções "Cor")
- `options[].values[]` precisa de pelo menos 1 valor
- `variants[].optionValues` deve ter o **mesmo comprimento** de `options[]` e estar **na mesma ordem** (índice 0 = primeira opção, índice 1 = segunda…)
- Cada combinação de `optionValues` é **única** dentro do grupo (não pode ter duas variantes "Preto+P"). Se mandar duplicada, retorna `422`.
- `keyword` por variante é opcional — backend gera sequencial (`1000`, `1001`, `1002`…) se não passar
- `images[]` da variante é a galeria adicional; `imageUrl` é a thumbnail principal

**Resposta 201:**
```json
{
  "data": {
    "id": "uuid-do-grupo",
    "name": "Camiseta Básica",
    "createdAt": "2026-04-25T12:00:00Z",
    "variants": [
      { "id": "uuid", "keyword": "1000", "optionValues": ["Preto", "P"] },
      { "id": "uuid", "keyword": "1001", "optionValues": ["Preto", "M"] },
      { "id": "uuid", "keyword": "1002", "optionValues": ["Azul Marinho", "P"] }
    ]
  }
}
```

**Erros típicos**:
- `422` "variant #2: variant optionValues length must match number of options" → o array `optionValues` da variante tem tamanho diferente do array `options`
- `422` "variant #3: option \"Cor\" does not have value \"Verde\"" → mandou um valor que não foi declarado em `options`
- `422` "variant #2: two variants share the same option value combination" → duas variantes com a mesma combinação
- `422` "keyword range exhausted (max 9999)" → loja já tem 9000 variantes (improvável)

---

## 4. Listagens — o que retorna

### `GET /products` — lista de variantes/produtos

Query params:
- `page`, `limit` (default 1, 20)
- `search` (busca em nome ou keyword)
- `sortBy` (`name|price|stock|created_at|updated_at|keyword`), `sortOrder` (`asc|desc`)
- `status[]` (multi: `active`, `inactive`)
- `externalSource[]` (multi: `manual`, `tiny`, `bling`, `shopify`)
- `priceMin`, `priceMax` (cents), `stockMin`, `stockMax`
- `hasLowStock=true`, `shippable=true`

**Cada item da lista (sem opções/imagens — vem do detail):**
```json
{
  "id": "uuid",
  "name": "Camiseta Básica — Preto / P",
  "keyword": "1000",
  "externalId": "",
  "externalSource": "manual",
  "price": 4990,
  "imageUrl": "https://...",
  "stock": 10,
  "active": true,
  "shipping": { ... },
  "shippable": true,
  "groupId": "uuid-do-grupo",         // null para produto simples
  "optionValues": [],                  // vazio na listagem por performance
  "images": [],                        // vazio na listagem por performance
  "createdAt": "...",
  "updatedAt": "..."
}
```

**Para mostrar variantes agrupadas no admin**: chame `GET /product-groups` e use o `variantsCount` para decidir se expande. Quando expandir, chame `GET /product-groups/:id` (que traz tudo: opções, valores, variantes, galerias).

### `GET /products/:id` — detalhe enriquecido

Mesma estrutura acima, **com** `optionValues` e `images` populados:
```json
{
  "id": "uuid",
  "name": "Camiseta Básica — Preto / P",
  "groupId": "uuid-do-grupo",
  "optionValues": [
    { "option": "Cor", "value": "Preto" },
    { "option": "Tamanho", "value": "P" }
  ],
  "images": [
    "https://cdn.livecart.com/p/cam-preto-p-detalhe-1.jpg",
    "https://cdn.livecart.com/p/cam-preto-p-detalhe-2.jpg"
  ],
  "imageUrl": "https://cdn.livecart.com/p/cam-preto-p.jpg",
  ... // demais campos
}
```

### `GET /product-groups` — lista de grupos (agregadores)

```json
{
  "data": [
    {
      "id": "uuid",
      "name": "Camiseta Básica",
      "description": "100% algodão pima",
      "externalId": "",
      "externalSource": "manual",
      "variantsCount": 6,
      "createdAt": "...",
      "updatedAt": "..."
    }
  ],
  "pagination": { "page": 1, "limit": 20, "total": 12, "totalPages": 1 }
}
```

### `GET /product-groups/:id` — detalhe completo do grupo

```json
{
  "data": {
    "id": "uuid",
    "name": "Camiseta Básica",
    "description": "...",
    "externalId": "",
    "externalSource": "manual",
    "options": [
      {
        "id": "uuid", "name": "Cor", "position": 0,
        "values": [
          { "id": "uuid", "value": "Preto", "position": 0 },
          { "id": "uuid", "value": "Azul Marinho", "position": 1 }
        ]
      },
      {
        "id": "uuid", "name": "Tamanho", "position": 1,
        "values": [
          { "id": "uuid", "value": "P", "position": 0 },
          { "id": "uuid", "value": "M", "position": 1 },
          { "id": "uuid", "value": "G", "position": 2 }
        ]
      }
    ],
    "groupImages": [
      { "id": "uuid", "url": "...", "position": 0 }
    ],
    "variants": [
      {
        "id": "uuid",
        "keyword": "1000",
        "optionValues": [
          { "option": "Cor", "value": "Preto" },
          { "option": "Tamanho", "value": "P" }
        ],
        "price": 4990,
        "stock": 10,
        "sku": "CAM-PT-P",
        "imageUrl": "https://...",
        "images": [{ "id": "uuid", "url": "...", "position": 0 }]
      }
    ],
    "createdAt": "...",
    "updatedAt": "..."
  }
}
```

---

## 5. Importação de Tiny com variantes

Validei contra o swagger oficial do Tiny v3 ([erp.tiny.com.br/public-api/v3/swagger](https://erp.tiny.com.br/public-api/v3/swagger)) — a implementação está alinhada. Resumo:

| Cenário no Tiny | O que o backend faz |
|---|---|
| Produto **simples** (`tipo=S`) | Cria 1 row em `products` com `groupId=null`. Comportamento atual, sem mudança. |
| Produto **com variações** (`tipo=V`) | Cria 1 `product_group` + 1 `product_option` por chave da `grade` Tiny + N `product_option_values` + 1 row em `products` por variação Tiny (cada uma com seu `external_id` próprio do filho). |
| Webhook Tiny no **filho** (variação muda) | Atualiza preço/estoque/imagem só daquela variante via `external_id`. |
| Webhook Tiny no **pai** | Re-sincroniza todas variantes (price/stock/active herdam do parent atualizado). |
| Variante nova adicionada no Tiny **depois** da primeira sync | Não importa automaticamente em v1 (precisa estender catálogo de option_values). Front pode adicionar variante manualmente via `POST /products` com `groupId`. |

Lojas que **não usam ERP** (cadastro 100% manual) continuam funcionando — produtos com `external_source = "manual"` nunca são enviados ao ERP.

---

## 6. Reaproveitando o que já existe no carrinho/live

**Nada muda no carrinho, live ou pedido.** Como cada variante é uma row em `products`:

- Adicionar ao carrinho via keyword: `POST /carts/:cartId/items` continua funcionando (a keyword é da variante)
- Reserva de estoque: `stock_reservations` reserva por `product_id` (= variante) ✓
- Pedido Tiny: o `external_id` do pedido continua sendo o do filho Tiny ✓
- Live event: matched_product_id continua sendo a variante ✓

---

## 7. Sugestão de UX para a tela "Novo produto"

```
┌────────────────────────────────────────────────────┐
│  Novo Produto                                       │
│                                                     │
│  Tipo:  ◉ Simples   ◯ Com variações                │
│                                                     │
│  ┌──────────────────────────────────────────────┐  │
│  │ Nome:           [_______________________]    │  │
│  │ Descrição:      [_______________________]    │  │
│  │                                               │  │
│  │ === Se "Simples" ===                         │  │
│  │ Preço (R$):     [____,__]                    │  │
│  │ Estoque:        [___]                        │  │
│  │ SKU:            [_______________________]    │  │
│  │ Imagem:         [upload]                     │  │
│  │ Dimensões:      [..] [..] [..] [..]          │  │
│  │                                               │  │
│  │ === Se "Com variações" ===                   │  │
│  │ Opções (max 3):                              │  │
│  │  • Cor       [Preto, Azul Marinho   ] [+]    │  │
│  │  • Tamanho   [P, M, G               ] [+]    │  │
│  │                                               │  │
│  │ Galeria do produto: [+ adicionar imagens]    │  │
│  │                                               │  │
│  │ Variantes (gerar matriz automaticamente):    │  │
│  │ ┌──────────────┬──────┬──────┬─────────┐    │  │
│  │ │ Cor / Tam    │ Preço│Estoq │ SKU     │    │  │
│  │ ├──────────────┼──────┼──────┼─────────┤    │  │
│  │ │ Preto · P    │ 49,90│  10  │ CAM-PT-P│    │  │
│  │ │ Preto · M    │ 49,90│   8  │ CAM-PT-M│    │  │
│  │ │ Azul · P     │ 49,90│   5  │ CAM-AZ-P│    │  │
│  │ │ ...          │      │      │         │    │  │
│  │ └──────────────┴──────┴──────┴─────────┘    │  │
│  │                                               │  │
│  │ [Cancelar]                          [Salvar] │  │
│  └──────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────┘
```

A matriz de variantes é gerada localmente pela UI fazendo o produto cartesiano de `options[].values[]` — backend só recebe a lista final em `variants[]`.

---

## 8. "Tela de produtos" — listagem

A listagem hoje retorna variantes "soltas" (cada row em `products`). Sugestão:

- Use `GET /product-groups` para a aba "Grupos" (mostra agregadores)
- Use `GET /products` (sem filtro de grupo) para a aba "Todos os SKUs" (mostra cada variante + simples como linha)
- Para mostrar a tabela "agrupada" estilo Shopify (linha-pai expansível com filhos), faça:
  1. `GET /product-groups` para os grupos
  2. `GET /products?search=...` para os simples (filtre `groupId=null` no front após resposta — backend hoje não tem esse filtro; podemos adicionar se for útil, me avisa)
  3. Expandir um grupo → `GET /product-groups/:id` para puxar suas variantes

---

## 9. Cadastro manual que "não funciona" hoje

A rota `POST /products` **funciona no backend**. O bug provavelmente está no front. Compare o payload que o front envia com o doc [products-create.md](products-create.md). Suspeitos mais comuns:

- **Naming**: `priceCents` ❌ → `price` ✅; `image_url` ❌ → `imageUrl` ✅
- **Falta `externalSource: "manual"`** no body
- **Mandando `price` como decimal** (ex.: `49.90`) em vez de centavos (`4990`)
- **`shipping` parcial** (só `weightGrams`, sem `heightCm/widthCm/lengthCm`) → backend retorna 400

Se persistir, manda o `curl -v` (request + response completos) que eu identifico em segundos.

---

## 10. O que **não** mudou (confirmação)

- Endpoint de adicionar ao carrinho
- Endpoint de checkout
- Quotação de frete
- Rotas do live event / matched products
- Webhooks de Mercado Pago / Tiny
- Stock reservations

Tudo isso opera por `product_id` que continua sendo uma row em `products`. Variante == produto vendável.

---

## 11. Perguntas frequentes

**P: Posso editar opções/valores de um grupo depois de criado?**
R: Em v1, só nome/descrição via `PUT /product-groups/:id`. Adicionar/remover opções ou valores requer chamar endpoints separados (ainda não expostos — me avisa se precisa). Para mudar, hoje é deletar o grupo e recriar.

**P: Posso ter uma variante sem `groupId` (produto simples) e depois "promover" para variante de um grupo novo?**
R: Não tem endpoint específico. Mas você pode criar o grupo e dar `PATCH` no produto enviando o `groupId` — isso ainda não está no `PUT /products/:id` atual. Me avisa se precisa.

**P: Quantas variantes um grupo pode ter?**
R: Sem limite duro. Limite implícito: keywords vão de 1000 a 9999 por loja, então uma loja toda comporta no máximo 9000 variantes/produtos.

**P: Imagens — qual é o fluxo de upload?**
R: O backend não faz upload — o front sobe para S3 (ou wherever) e manda a URL pronta. A rota `POST /products/:id/images` recebe `{ url, position }` e registra no banco.

**P: Como filtrar produtos de um grupo específico?**
R: Hoje não tem `?groupId=` no `GET /products`. Use `GET /product-groups/:id` que traz `variants[]` direto. Posso adicionar o filtro se ficar incômodo.

---

Qualquer coisa: alissondahlem@gmail.com / no Slack / me responde aqui.
