# Arquitetura de Integrações — Plataforma de Live Commerce

> Documento de referência interno. Define escopo, responsabilidades, contratos e regras de prioridade de cada categoria de integração.
>
> **Versão:** 1.0
> **Status:** Draft inicial — revisar trimestralmente

---

## Sumário

1. [Princípios fundamentais](#1-princípios-fundamentais)
2. [Categorias de integração](#2-categorias-de-integração)
3. [Responsabilidades por categoria](#3-responsabilidades-por-categoria)
4. [Interface padrão (contrato comum)](#4-interface-padrão-contrato-comum)
5. [Limites e fronteiras](#5-limites-e-fronteiras)
6. [Hierarquia de prioridade entre integrações com responsabilidades equivalentes](#6-hierarquia-de-prioridade-entre-integrações-com-responsabilidades-equivalentes)
7. [Regras de conflito e fallback](#7-regras-de-conflito-e-fallback)
8. [Checklist para nova integração](#8-checklist-para-nova-integração)

---

## 1. Princípios fundamentais

Estes princípios são inegociáveis. Toda decisão de integração deve ser validada contra eles.

### 1.1. Single Source of Truth (SSoT)

Cada dado de domínio tem **uma única fonte de verdade**. Nunca duas integrações são responsáveis pelo mesmo dado simultaneamente. Quando há sobreposição (ex: produto existe no ERP e no e-commerce), a hierarquia de prioridade define quem manda.

### 1.2. Nosso sistema orquestra, não armazena lógica de terceiros

Nosso sistema é o **maestro**. Cada integração externa é um **instrumento** com função específica. A lógica de negócio (carrinho da live, captura de intenção, fluxo de venda) mora no nosso sistema. Integrações apenas fornecem capacidades atômicas.

### 1.3. Idempotência por padrão

Toda operação que cruza fronteira de integração deve ser idempotente. Webhooks duplicados, retries automáticos e reentregas são comportamento esperado, não exceção.

### 1.4. Fallback obrigatório em integrações críticas

Se uma integração externa cair, o sistema deve continuar capturando vendas. Resiliência > consistência imediata, dentro do razoável.

### 1.5. Configuração por cliente, não por código

Cada lojista tem um setup diferente (alguns só ERP, outros só e-commerce, alguns ambos). A combinação de integrações ativas é configuração, não fork de código.

---

## 2. Categorias de integração

Definimos **5 categorias** de integração externa. Toda integração nova deve se encaixar em uma e apenas uma destas categorias. Se não couber, é sinal de que estamos saindo do escopo do produto.

| ID | Categoria | Exemplos atuais | Exemplos futuros |
|---|---|---|---|
| **ERP** | Sistema de gestão | Tiny | Bling, Eccosys, Olist |
| **ECOM** | Plataforma de e-commerce | — | Nuvemshop, Tray, Shopify |
| **PAY** | Gateway de pagamento | Mercado Pago, Pagar.me | Asaas, Stone, Cielo |
| **SHIP** | Cálculo de frete e logística | Melhor Envio, SmartEnvios | Frenet, Kangu, Loggi |
| **SOCIAL** | Rede social / canal de live | Instagram | Facebook, TikTok, Kwai |

---

## 3. Responsabilidades por categoria

### 3.1. ERP

**Domínio de verdade:** estoque, produto (cadastro mestre), nota fiscal, pedido consolidado pós-venda.

**Responsabilidades exclusivas:**

- Cadastro mestre de produtos (SKU, variações, preço base, custo, dimensões, peso)
- Controle de estoque real (quantidade disponível, reservas)
- Emissão de nota fiscal (NFe / NFCe)
- Recepção de pedidos pós-pagamento confirmado
- Atualização de status de fulfillment (separado, expedido, entregue)

**Não é responsabilidade do ERP no nosso sistema:**

- Processamento de pagamento
- Cálculo de frete em tempo real (mesmo que o ERP tenha módulo)
- Gestão do carrinho ao vivo
- Autenticação de comprador final
- Captura ou interpretação de comentários de live

### 3.2. ECOM (plataforma de e-commerce)

**Domínio de verdade:** loja virtual pública, catálogo público, checkout próprio (quando o lojista usa).

**Responsabilidades exclusivas:**

- Fonte alternativa de catálogo (quando o lojista não tem ERP)
- Sincronização de pedidos consolidados (registro espelho do pedido fechado)
- Cadastro de clientes finais da loja (CRM da plataforma)

**Não é responsabilidade da ECOM no nosso sistema:**

- Fonte de verdade de estoque quando há ERP presente
- Processamento de pagamento da live
- Gestão da experiência da live em si

### 3.3. PAY (gateway de pagamento)

**Domínio de verdade:** transação financeira, antifraude, conciliação.

**Responsabilidades exclusivas:**

- Processamento de pagamento (Pix, cartão, boleto)
- Tokenização de cartão para recompra
- Antifraude
- Repasse ao lojista (split, conta de destino)
- Estorno e tratamento de chargeback
- Emissão de webhook de status (pago, pendente, recusado, estornado, reembolsado)

**Não é responsabilidade do PAY no nosso sistema:**

- Conhecimento de produto, estoque ou pedido
- Autenticação de usuário do nosso sistema
- Decisão de fluxo após confirmação de pagamento

### 3.4. SHIP (frete)

**Domínio de verdade:** cotação de frete, geração de etiqueta, rastreio.

**Responsabilidades exclusivas:**

- Cotação em tempo real (CEP origem, CEP destino, peso, dimensões)
- Geração de etiqueta após pedido fechado
- Rastreio (status de envio)
- Lista de transportadoras disponíveis (PAC, SEDEX, Jadlog, etc.)

**Não é responsabilidade da SHIP no nosso sistema:**

- Controle de estoque
- Processamento de pagamento (incluindo pagamento da etiqueta — usa crédito do lojista na própria plataforma de frete)
- Conhecimento do pedido além de peso, dimensão e destino

### 3.5. SOCIAL (rede social / canal de live)

**Domínio de verdade:** transmissão ao vivo, comentários, audiência.

**Responsabilidades exclusivas:**

- Stream da live (não transmitimos vídeo, apenas observamos)
- Feed de comentários em tempo real
- Identificação pública do usuário (handle, foto, ID público)
- Envio de mensagem direta (DM) ao usuário, sujeito a policies da plataforma

**Não é responsabilidade da SOCIAL no nosso sistema:**

- Processamento de pedido ou pagamento
- Fonte de catálogo confiável (mesmo que Instagram Shop exista)
- Cadastro mestre de cliente final

---

## 4. Interface padrão (contrato comum)

Toda integração, independente de categoria, deve implementar um conjunto mínimo de capacidades para ser orquestrada corretamente pelo nosso sistema. Esta seção define o contrato lógico.

### 4.1. Capacidades transversais (toda integração precisa ter)

| Capacidade | Descrição |
|---|---|
| **Autenticação por cliente** | Suporte a credenciais por lojista (OAuth token, API key, etc.). Nunca uma única credencial global. |
| **Health check** | Endpoint ou método que indique se a integração está operante para o cliente em questão. |
| **Idempotência** | Toda operação de escrita aceita uma chave de idempotência (idempotency_key) para evitar duplicação em retries. |
| **Tratamento de erro padronizado** | Erros mapeados para uma taxonomia comum: `auth_error`, `rate_limit`, `not_found`, `validation_error`, `external_unavailable`, `unknown`. |
| **Observabilidade** | Toda chamada externa registra: cliente, integração, operação, latência, status, payload (sanitizado). |
| **Versionamento** | Cada integração declara a versão da API externa que está consumindo. Mudanças de versão são explícitas. |
| **Configuração de retry** | Política de retry definida por tipo de operação (idempotente vs. não idempotente). |

### 4.2. Contrato por categoria

Cada categoria precisa expor um conjunto específico de capacidades. Implementações concretas (Tiny, Bling, etc.) devem aderir a essa interface.

#### 4.2.1. Interface ERP

**Operações de leitura:**

- `listar_produtos(filtro?, paginação)` → lista de produtos com SKU, preço, estoque, variações
- `buscar_produto(sku)` → detalhe completo
- `consultar_estoque(sku, quantidade?)` → quantidade disponível em tempo real
- `listar_pedidos(filtro?)` → para reconciliação
- `buscar_status_pedido(pedido_id)` → status de fulfillment

**Operações de escrita:**

- `criar_pedido(dados_pedido, idempotency_key)` → pedido criado no ERP após pagamento
- `cancelar_pedido(pedido_id, motivo)` → em caso de estorno

**Eventos consumidos (webhooks recebidos do ERP):**

- `produto.atualizado` → reflete mudança de preço, estoque, variação
- `pedido.status_alterado` → atualiza painel do lojista
- `nfe.emitida` → confirma emissão fiscal

#### 4.2.2. Interface ECOM

**Operações de leitura:**

- `listar_produtos(filtro?, paginação)`
- `buscar_produto(id)`
- `consultar_estoque(id)` *(usado apenas se não houver ERP)*

**Operações de escrita:**

- `criar_pedido(dados_pedido, idempotency_key)` *(modo espelho — opcional)*

**Eventos consumidos:**

- `produto.atualizado`
- `pedido.atualizado`

#### 4.2.3. Interface PAY

**Operações de escrita:**

- `criar_intencao_pagamento(valor, comprador, método, idempotency_key)` → retorna identificador da transação e dados para finalização (QR Pix, link, etc.)
- `consultar_pagamento(transacao_id)` → status atual
- `estornar_pagamento(transacao_id, valor?)` → estorno total ou parcial
- `tokenizar_cartao(dados_cartao)` → token persistido para recompra
- `cobrar_com_token(token, valor, idempotency_key)` → cobrança recorrente

**Eventos consumidos (webhooks do gateway):**

- `pagamento.confirmado` → dispara orquestração pós-pagamento
- `pagamento.recusado`
- `pagamento.estornado`
- `pagamento.chargeback`

**Garantias esperadas:**

- Webhook é a única fonte de verdade para mudança de status. Nunca confiar no retorno síncrono.
- Toda confirmação deve ser validada via assinatura/HMAC.

#### 4.2.4. Interface SHIP

**Operações de leitura:**

- `cotar_frete(cep_origem, cep_destino, peso, dimensões, valor_segurado?)` → lista de opções (transportadora, prazo, preço)

**Operações de escrita:**

- `gerar_etiqueta(dados_envio, idempotency_key)` → etiqueta + código de rastreio
- `cancelar_etiqueta(etiqueta_id)` → quando aplicável

**Eventos consumidos:**

- `envio.status_alterado` → reflete no painel do lojista

#### 4.2.5. Interface SOCIAL

**Operações de leitura:**

- `obter_lives_ativas(conta)` → lista de lives em andamento
- `obter_comentarios(live_id, desde?)` → polling ou streaming

**Operações de escrita:**

- `enviar_dm(usuario_id, mensagem, idempotency_key)` → sujeito a policies

**Eventos consumidos (webhooks ou polling):**

- `comentario.recebido` → entrada principal do nosso bot
- `live.iniciada`
- `live.encerrada`

---

## 5. Limites e fronteiras

Esta seção lista, para cada categoria, **o que está dentro e fora** do escopo. Use como guarda-costas quando houver dúvida sobre ampliar uma integração.

### 5.1. Limites do ERP

✅ **Dentro:**
- Sincronização de catálogo (leitura)
- Consulta de estoque em tempo real
- Criação de pedido após pagamento
- Recepção de status de fulfillment

❌ **Fora:**
- Substituir o ERP em qualquer função (não somos ERP)
- Disparar emissão fiscal manualmente (o ERP que controla)
- Editar produtos pelo nosso painel (cadastro fica no ERP)
- Substituir relatórios financeiros do ERP

### 5.2. Limites da ECOM

✅ **Dentro:**
- Importar catálogo quando o lojista não tem ERP
- Espelhar pedido fechado para histórico unificado
- Sincronizar cliente final

❌ **Fora:**
- Usar checkout da plataforma para venda da live (sempre nosso checkout)
- Substituir ERP quando ele existe
- Construir loja virtual paralela
- Concorrer com a plataforma em features (ex: cupom, programa de fidelidade)

### 5.3. Limites do PAY

✅ **Dentro:**
- Processar pagamento da venda da live
- Tokenizar cartão para recompra
- Confiar no antifraude do gateway
- Receber webhooks de status

❌ **Fora:**
- Construir antifraude próprio
- Armazenar dados sensíveis de cartão (PCI fica com o gateway)
- Mediar disputa de chargeback
- Substituir conciliação financeira do lojista

### 5.4. Limites da SHIP

✅ **Dentro:**
- Cotar frete em tempo real durante checkout
- Gerar etiqueta após pedido fechado (opcional, configurável)
- Refletir rastreio no painel

❌ **Fora:**
- Negociar contrato direto com transportadoras
- Gerenciar crédito do lojista na plataforma de frete (o lojista usa a conta dele)
- Substituir SAC de problemas de entrega (lojista resolve com a transportadora)
- Gerenciar logística reversa complexa

### 5.5. Limites da SOCIAL

✅ **Dentro:**
- Capturar comentários da live
- Identificar usuário pelo handle
- Enviar DM de recuperação (sujeito a policies)

❌ **Fora:**
- Transmitir vídeo (lojista usa app nativo)
- Gerenciar conteúdo da live (edição, agendamento, descrição)
- Substituir Instagram Shop como vitrine pública
- Construir feed/timeline próprio dentro do nosso app
- Burlar rate limits ou policies da plataforma

---

## 6. Hierarquia de prioridade entre integrações com responsabilidades equivalentes

Quando o lojista tem **mais de uma integração ativa** que poderia fornecer o mesmo dado, a hierarquia abaixo define quem ganha. Isso é configurável por cliente, mas o default é este.

### 6.1. Conflito: ERP vs. ECOM (catálogo e estoque)

**Cenários possíveis:**

| Setup do lojista | Fonte de produto | Fonte de estoque | Onde grava pedido |
|---|---|---|---|
| Apenas ERP | ERP | ERP | ERP |
| Apenas ECOM | ECOM | ECOM | ECOM |
| ERP + ECOM | **ERP** | **ERP** | ERP (ECOM espelha por fora ou via nosso sistema) |
| Nenhum | Nosso sistema (cadastro manual) | Nosso sistema | Nosso sistema |

**Regra:** ERP sempre vence ECOM em produto e estoque, porque ERP é desenhado para esse domínio. ECOM existe como fallback quando não há ERP.

**Justificativa:** ERP costuma ser a fonte real do estoque físico. Lojistas que usam ambos geralmente já configuram o ERP como master e a ECOM como espelho.

### 6.2. Conflito: múltiplos PAY ativos

**Default:** o lojista define **um gateway primário por método de pagamento**. Por exemplo:

- Pix → Asaas
- Cartão → Pagar.me
- Boleto → Mercado Pago

**Regra:** o sistema escolhe o gateway baseado no método selecionado pelo comprador, conforme configuração do lojista. Não há fallback automático entre gateways em caso de falha — se o gateway primário falhar, a transação falha (e o comprador escolhe outro método).

**Justificativa:** trocar de gateway no meio de uma transação levanta questões de antifraude, conciliação e taxas. Risco maior que benefício.

### 6.3. Conflito: múltiplos SHIP ativos

**Default:** o sistema **consulta todos os SHIP ativos em paralelo** e apresenta as cotações unificadas ao comprador, ordenadas por preço ou prazo (configurável).

**Regra:** não há "vencedor" — todas as opções aparecem. Se uma plataforma de frete falhar na cotação, o sistema apresenta apenas as outras (sem bloquear a venda).

**Justificativa:** frete é uma decisão do comprador, não do sistema. Mais opções = mais conversão.

### 6.4. Conflito: múltiplos SOCIAL ativos

**Regra:** não há conflito real — cada live acontece em uma única plataforma SOCIAL por vez. O sistema escuta a plataforma onde a live está ativa.

**Caso especial:** se o lojista transmite simultaneamente em duas plataformas (raro), o sistema trata como duas lives separadas, com carrinhos independentes por usuário/plataforma.

### 6.5. Conflito: ERP, ECOM e SHIP em emissão de etiqueta

**Default:** SHIP é sempre quem gera etiqueta. ERP e ECOM podem ter módulos de logística próprios, mas **não os usamos**.

**Regra:** SHIP é a fonte única para etiqueta e rastreio.

**Justificativa:** lojista médio costuma centralizar frete numa plataforma agregadora (Melhor Envio). Usar o módulo do ERP ou da ECOM duplicaria dado e fragmentaria a operação.

---

## 7. Regras de conflito e fallback

### 7.1. Reserva de estoque durante a live

**Decisão:** estoque é **reservado no momento do checkout**, não no momento do comentário.

**Motivo:** comentário é intenção, checkout é compromisso. Reservar no comentário causa "estoque fantasma" se o comprador não finaliza.

**Implementação:**

- No comentário: validamos disponibilidade (sem reservar)
- No checkout iniciado: reserva soft de N minutos
- No pagamento confirmado: baixa real no ERP

### 7.2. Cache de produto e estoque

**Decisão:** cache com TTL curto (sugestão: 30–60s para estoque, 5–10min para produto).

**Motivo:** lives consomem estoque em segundos. TTL longo causa oversell.

**Fallback:** se o ERP estiver lento ou fora, o sistema usa o cache mais recente e marca a leitura como "stale". Compras feitas com dado stale são reconciliadas após o ERP voltar.

### 7.3. ERP indisponível durante a live

**Decisão:** o sistema continua aceitando vendas usando cache de produto/estoque. Pedidos ficam em **fila de envio ao ERP** e são processados quando o ERP volta.

**Risco aceito:** oversell pontual. **Risco evitado:** perder venda durante live (impacto financeiro maior).

### 7.4. Gateway falha após cobrança

**Cenário:** cobrança foi feita, mas webhook de confirmação não chegou.

**Decisão:** sistema faz polling no gateway a cada N minutos por até X horas para reconciliar. Pedido fica em estado `pagamento_pendente_confirmacao`.

### 7.5. Webhook duplicado

**Decisão:** toda operação tem chave de idempotência. Webhook duplicado é detectado e descartado.

### 7.6. SOCIAL com rate limit ou queda da API

**Decisão:** sistema oferece **fallback de captura manual** — lojista pode digitar "vendido pra @fulana — produto X" e o sistema processa.

**Motivo:** dependência de Meta é risco existencial. Sempre ter rota manual.

---

## 8. Checklist para nova integração

Toda nova integração deve passar por esta checklist antes de ir pra produção.

### 8.1. Definição de escopo

- [ ] A integração se encaixa em uma das 5 categorias definidas?
- [ ] As responsabilidades estão claras e não duplicam outra integração existente?
- [ ] Os limites (o que está fora) estão documentados?
- [ ] Foi atualizada a hierarquia de prioridade, se houver sobreposição?

### 8.2. Implementação técnica

- [ ] Implementa todas as capacidades transversais (seção 4.1)?
- [ ] Implementa o contrato da categoria (seção 4.2)?
- [ ] Suporta múltiplas credenciais (uma por lojista)?
- [ ] Tem health check funcional?
- [ ] Operações de escrita aceitam idempotency key?
- [ ] Erros são mapeados para a taxonomia comum?
- [ ] Webhooks (se houver) são validados via assinatura?
- [ ] Política de retry definida e documentada?

### 8.3. Operação

- [ ] Logging e métricas instrumentados?
- [ ] Alertas configurados para queda da integração?
- [ ] Runbook de troubleshooting escrito?
- [ ] Onboarding do lojista documentado (como conectar)?
- [ ] Plano de fallback definido para indisponibilidade?

### 8.4. Negócio

- [ ] % do ICP que usa essa integração foi estimado?
- [ ] Custo de manutenção justifica a adoção esperada?
- [ ] Time de CS e Vendas foi treinado?

---

## Anexo A — Matriz de responsabilidade resumida

| Capacidade | ERP | ECOM | PAY | SHIP | SOCIAL | **Sistema** |
|---|:-:|:-:|:-:|:-:|:-:|:-:|
| Cadastro de produto | Master | Alt. | — | — | — | Cache |
| Estoque real | Master | Alt. | — | — | — | Cache |
| Captura de comentário | — | — | — | — | Master | Orquestra |
| Carrinho da live | — | — | — | — | — | Master |
| Catálogo da live | — | — | — | — | — | Master |
| Cotação de frete | — | — | — | Master | — | Orquestra |
| Pagamento | — | — | Master | — | — | Orquestra |
| Pedido consolidado | Master | Sync | — | — | — | Orquestra |
| Nota fiscal | Master | — | — | — | — | — |
| Etiqueta de envio | — | — | — | Master | — | Orquestra |
| Cliente final | Sync | Sync | — | — | Identifica | Master |
| Analytics da live | — | — | — | — | — | Master |

**Legenda:**
- **Master:** fonte de verdade primária
- **Alt.:** fonte alternativa (usada apenas quando o master não existe)
- **Sync:** recebe sincronização espelho, não é fonte de verdade
- **Cache:** mantém cópia temporária por performance
- **Orquestra:** coordena mas não armazena
- **Identifica:** fornece identificador externo, não cadastro completo
- **—:** não envolvido

---

## Anexo B — Glossário

- **SSoT (Single Source of Truth):** fonte única de verdade para um dado
- **ERP:** Enterprise Resource Planning — sistema de gestão integrado
- **ECOM:** plataforma de e-commerce
- **PAY:** gateway de pagamento
- **SHIP:** integração de frete
- **SOCIAL:** plataforma de rede social que hospeda a live
- **Idempotência:** propriedade que garante que uma operação executada múltiplas vezes produz o mesmo resultado
- **Webhook:** notificação HTTP enviada por um sistema externo quando um evento ocorre
- **TTL (Time To Live):** tempo de validade de um dado em cache
- **Stale:** dado em cache que pode estar desatualizado
- **Master:** sistema dono primário de um dado
- **Espelho / Sync:** cópia secundária de um dado, sincronizada do master

---

## Histórico de revisões

| Versão | Data | Autor | Mudanças |
|---|---|---|---|
| 1.0 | _preencher_ | _preencher_ | Versão inicial |
