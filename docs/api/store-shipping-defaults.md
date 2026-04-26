# Store — Defaults de embalagem (fallback de frete na importação ERP)

> Doc complementar para a tela de configurações da loja (`Settings → Envio`).

## Por que existe

Quando o Tiny tem o **peso** cadastrado mas não as **dimensões** (caso comum em loja de roupa, onde o merchant pesa cada SKU mas usa sempre a mesma caixa), produtos importados ficavam com `shipping: null` e `shippable: false` — frete não calculava até alguém editar manualmente cada produto.

A loja agora tem 3 campos opcionais (`heightCm`, `widthCm`, `lengthCm`) que servem como **fallback** durante a importação/sync ERP. Se a loja configurou esses 3, o backend completa qualquer produto Tiny que só tenha peso, criando um perfil de frete completo automaticamente.

## Endpoint

`PUT /api/v1/stores/:storeId/shipping-defaults` (já existia — ganhou 3 campos novos no body).

Também existe `PUT /api/v1/stores/me/shipping-defaults` para a loja do usuário autenticado.

**Body:**
```jsonc
{
  "packageWeightGrams": 250,           // já existia — fallback de peso (raramente usado)
  "packageFormat": "box",              // já existia — box | roll | letter
  "heightCm": 5,                       // NOVO — opcional
  "widthCm":  25,                      // NOVO — opcional
  "lengthCm": 30                       // NOVO — opcional
}
```

**Validações** dos campos novos:
- Cada um, se enviado, deve ser **inteiro > 0**
- Os 3 são **all-or-nothing**: se algum vier `null`/ausente, o backend persiste os 3 como `NULL` e o fallback fica desativado

**Resposta** (`StoreOutput.shippingDefaults`):
```jsonc
{
  "packageWeightGrams": 250,
  "packageFormat": "box",
  "heightCm": 5,                       // null quando fallback desativado
  "widthCm":  25,
  "lengthCm": 30
}
```

## UX sugerida

Na tela de Envio, abaixo dos campos atuais, uma seção opcional:

```
┌──────────────────────────────────────────────────────────┐
│ Caixa padrão para importações automáticas (opcional)    │
│                                                           │
│ Quando você importa produtos do ERP que só têm peso,    │
│ usamos estas dimensões como padrão para que o cálculo   │
│ de frete funcione sem você precisar editar cada produto.│
│                                                           │
│ Altura:      [___] cm                                    │
│ Largura:     [___] cm                                    │
│ Comprimento: [___] cm                                    │
│                                                           │
│ [Limpar defaults]                                        │
└──────────────────────────────────────────────────────────┘
```

- Se o user limpar os 3, manda `null` para os 3 — fallback desativa.
- Mostre o exemplo "Para camisetas dobradas, algo como 5×25×30 cm é comum."

## Como funciona no import (referência)

Ordem de precedência (por produto / variante):
1. **ERP retornou shipping completo** (peso + dimensões) → usa direto.
2. **Variação sem dimensões + pai com dimensões** → herda do pai (já implementado antes).
3. **ERP retornou só peso + loja tem H/W/L config'd** → completa com defaults da loja.
4. **ERP retornou nada + loja tem peso + H/W/L** → perfil 100% sintético da loja.
5. **Senão** → produto entra com `shipping: null` e `shippable: false`.

Aplica em: `POST /integrations/:id/products/:tinyProductId/import`, `POST /integrations/:id/products/:productId/sync`, e webhook automático do Tiny.

Logs informativos (`completed shipping with store defaults`) aparecem quando o fallback é acionado.
