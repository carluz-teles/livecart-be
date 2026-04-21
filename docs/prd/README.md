# PRDs - LiveCart

Este diretorio contem os Product Requirements Documents (PRDs) para as features do LiveCart.

## 🎯 Features CORE (MVP)

Estas sao as features essenciais para o funcionamento do produto:

| # | Feature | Descricao | PRD | Status |
|---|---------|-----------|-----|--------|
| 1 | Resposta Automatica em Tempo Real | Responder comentarios com link em < 3s | [001-resposta-automatica.md](./001-resposta-automatica.md) | 🔴 Critico |
| 2 | Checkout Durante a Live | Checkout disponivel a qualquer momento | [002-checkout-durante-live.md](./002-checkout-durante-live.md) | 🔴 Critico |
| 3 | Carrinho Incremental | Acumular produtos via comentarios | [003-carrinho-incremental.md](./003-carrinho-incremental.md) | 🔴 Critico |
| 4 | Modo Live (Controle de Contexto) | Painel do vendedor para produto ativo | [004-modo-live.md](./004-modo-live.md) | 🔴 Critico |
| 5 | Atribuicao de Receita | Tracking de GMV por live | [005-atribuicao-receita.md](./005-atribuicao-receita.md) | 🔴 Critico |

## 📊 Metricas de Sucesso

### Conversao
- % comentarios → compra
- Tempo medio ate compra

### Produto
- Numero medio de itens por carrinho
- Taxa de abandono

### Negocio
- GMV por live
- Receita incremental

## ⚠️ Riscos Identificados

### Tecnicos
- Latencia alta → perda de conversao
- Inconsistencia de carrinho
- Duplicidade de eventos

### Produto
- Spam de mensagens
- Erro de interpretacao de comentarios
- Friccao no checkout

## 🔄 Fluxo Tecnico (Alto Nivel)

```
webhook recebe comentario
        ↓
evento enviado para fila
        ↓
processor:
  ├── identifica usuario
  ├── identifica produto (contexto)
  ├── atualiza carrinho
  ├── gera checkout link
  └── envia notificacao
        ↓
tracking de eventos
```

---

## 🟡 Features Secundarias (Pos-MVP)

| # | Feature | PRD |
|---|---------|-----|
| 6 | Cupons de Desconto | [006-cupons-desconto.md](./006-cupons-desconto.md) |
| 7 | Integracao Transportadoras | [007-integracao-transportadoras.md](./007-integracao-transportadoras.md) |
| 8 | Nota Fiscal Automatica | [008-nota-fiscal.md](./008-nota-fiscal.md) |
| 9 | Plataformas Adicionais | [009-plataformas-adicionais.md](./009-plataformas-adicionais.md) |
| 10 | Categorias de Produtos | [010-categorias-produtos.md](./010-categorias-produtos.md) |
| 11 | Exportacao de Relatorios | [011-exportacao-relatorios.md](./011-exportacao-relatorios.md) |

## 🟢 Nice to Have (Futuro)

| # | Feature | PRD |
|---|---------|-----|
| 12 | Dominio Personalizado | [012-dominio-personalizado.md](./012-dominio-personalizado.md) |
| 13 | Programa de Fidelidade | [013-programa-fidelidade.md](./013-programa-fidelidade.md) |
| 14 | Analytics Avancado | [014-analytics-avancado.md](./014-analytics-avancado.md) |
