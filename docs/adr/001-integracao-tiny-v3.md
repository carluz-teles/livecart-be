# ADR 001 — Integração LiveCart ↔ Olist/Tiny v3

- **Status:** proposto — Caminho A verificado em 26/08, ver §5.2
- **Data:** 26/08/2026
- **Contexto:** Fase 0 concluída. Base empírica em [`RECON.md`](../RECON.md) §7bis e §8.0,
  mapa do código em [`AUDIT.md`](../AUDIT.md).

> Convenção de procedência, igual à do `RECON.md`: `[EMPÍRICO]` foi medido contra a conta
> real e a data está dita; `[DOC]` é a documentação oficial da Olist; `[SWAGGER]` é só o
> contrato declarado e **nunca** prova comportamento; `[ABERTO]` é o que ainda não sabemos.

---

## 1. O que motivou

O fluxo atual reserva estoque com **lançamento manual de saída** (`POST /estoque` tipo `S`)
a cada comentário, estorna tudo no pagamento e só então cria o pedido. Os defeitos são
conhecidos: não existe documento no ERP até o fim, o par estorno→criação abre janela de
inconsistência, e mexer em saldo físico dispara webhook de estoque que realimenta a fila.

A Fase 0 acrescentou uma razão que ninguém tinha medido, e ela sozinha decide o ADR.

---

## 2. 🔴 A descoberta que decide tudo

`[EMPÍRICO 26/08]` Live simulada: **150 comentários, 60 compradoras, 7 produtos**, disparados
de uma vez contra o ambiente local ligado à conta real.

| | |
|---|---|
| carrinhos criados | 83 |
| unidades admitidas | 215 |
| reservas tentadas no ERP | **170** |
| **confirmadas** | **55 (32%)** |
| **`unconfirmed`** | **115 (68%)** |
| erros 429 | **zero** |
| erros por timeout | **115** |

**68% das reservas de uma live moderada terminam em `unconfirmed`** — o estado que,
por desenho, nunca re-tenta e **trava a finalização do carrinho pago** até um humano
intervir. E o ERP nunca disse não: **não houve um único 429**. As chamadas morreram no
prazo de 90 s enquanto esperavam a fila interna.

`[EMPÍRICO 26/08]` Conferindo o saldo real no Tiny depois: das 144 unidades em dúvida,
**praticamente nenhuma chegou** (o baixado no ERP, 65, bate com o que confirmamos, 69,
dentro do ruído de ±4 das promoções concorrentes durante a medição). Ou seja: eram seguras de repetir, e o sistema
não tinha como saber.

Isto não é um caso de borda. É o comportamento normal do desenho atual sob a carga para a
qual o produto existe.

---

## 3. O teto real, e por que ele é o eixo do desenho

`[EMPÍRICO 25–26/08]` A API v3 impõe **dois baldes independentes**, e o 429 diz qual estourou
pelo `X-Ratelimit-Limit`:

| balde | limite | janela |
|---|---|---|
| rajada | **4** requisições | 1 s |
| sustentado | **30** requisições | 60 s |

`[DOC]` **O limite é por CONTA, não por aplicativo** — várias lojas no mesmo Tiny dividem o
orçamento — e **escrita é mais restrita que leitura**. Não existe `Retry-After`; o único
sinal é `X-Ratelimit-Reset`.

`[EMPÍRICO 26/08]` **Os headers nem sempre vêm.** Em 40 escritas sequenciais, `X-Ratelimit-*`
veio ausente; na rajada, veio preenchido. Backoff que dependa do header precisa de fallback.

**Orçamento de uma live de 3 h: ~5.400 chamadas para tudo.** Uma escrita por item adicionado
não cabe, e o balde de 4/s limita nossa concorrência contra o ERP a quatro escritas por
segundo, aconteça o que acontecer na live.

---

## 4. O que o ERP protege — e o que não

`[EMPÍRICO 26/08]` Quatro `POST /estoque` tipo `S` simultâneos sobre **saldo 1**:
as quatro aceitas, **saldo final −3**. **O lançamento manual não valida saldo, nem sob
concorrência.**

`[EMPÍRICO 26/08]` `lancar-estoque` de um pedido de 10 unidades sobre saldo 1:
**400 — "Não é possível integrar o estoque deste pedido pois o saldo em estoque…"**.
**O pedido valida; o lançamento manual não.**

É o argumento mais forte a favor da migração: sair do movimento manual para o pedido
**ganha uma trava de saldo no lado do ERP** que hoje simplesmente não existe.

### 4.1 Três corridas que o ERP perde

`[EMPÍRICO 26/08]`

| corrida | resultado |
|---|---|
| 3× `lancar-estoque` simultâneos no mesmo pedido | 204 / 400 "Estoque já lançado." / 204 → **baixou 2× um pedido de 1 item** |
| 3× `estornar-estoque` simultâneos | três 204 → **saldo inflado em +2** |
| 3× `PUT /itens` simultâneos (qtd 2, 5, 9) | três 204 → grade final com **DUAS linhas** (2 e 9) |

A guarda de "estoque já lançado" **não é atômica**: é check-then-act, e perde a corrida.
O `PUT /itens` **não é last-write-wins** — PUTs concorrentes produzem uma grade corrompida.

**Consequência não negociável: toda escrita no ERP relativa a um mesmo pedido tem de ser
serializada do nosso lado.** A fila por pedido deixou de ser boa prática e virou requisito.

### 4.2 O nosso gate, esse, funciona

`[EMPÍRICO 26/08]` Estoque **1 no Tiny e 1 no local**, **15 comentários simultâneos**:

```
admitidas: 1     em fila: 14     carrinhos: 15     reservas no ERP: 1
```

O `UPDATE … WHERE stock >= $2` (`product.sql:55-60`) segurou. O split otimista chegou a ler
`stock=1` em três workers ao mesmo tempo, mas o decremento atômico converteu dois deles em
fila. **A admissão local é correta sob concorrência e continua sendo a autoridade de gate.**

---

## 5. Decisão

### 5.1 O pedido passa a ser o documento, desde o primeiro item

Criar o pedido de venda no Tiny já na primeira admissão e mutá-lo com
`PUT /pedidos/{id}/itens`.

Três achados tornam isso viável, e nenhum deles era conhecido antes da Fase 0:

1. `[EMPÍRICO 26/08]` **Só `idContato` é obrigatório** no `POST /pedidos`. `itens` e `data`
   são opcionais — dá para criar o pedido **vazio** e preenchê-lo depois.
2. `[EMPÍRICO 26/08]` **`PUT /itens` preserva `valorFrete` e `valorDesconto`**; recalcula só
   o total de produtos. Não é preciso cancelar e recriar para manter o frete.
3. `[EMPÍRICO 26/08]` O marcador é âncora de idempotência confiável: gravado com
   `POST /pedidos/{id}/marcadores` (corpo é **array puro**), legível em **segundos**, e
   **acumula** em vez de substituir.

### 5.2 O Caminho A está VERIFICADO e é o alvo

`[EMPÍRICO 26/08 — módulo de reserva instalado na conta de teste]` A pergunta que
bloqueava este ADR foi medida, e a resposta é favorável em todos os pontos:

| teste | resultado |
|---|---|
| criar pedido | `saldo` **inalterado**, `reservado 0→2`, `disponivel 42→40` |
| **`PUT /itens` num pedido apenas RESERVADO** | **204** — e `reservado` foi 2→5: o PUT **reajusta a reserva** |
| cancelar (`situacao=2`) | `reservado` volta a 0, saldo intocado, **sozinho** |
| `lancar-estoque` sobre a reserva | `saldo −3` e `reservado −3` — converte limpo, **sem contar duas vezes** |

**A suposição sobre a qual todo o fluxo alvo se apoiava é verdadeira.** O pedido pode
nascer na primeira admissão, ser mutado item a item com `PUT /itens`, e a reserva
acompanha a grade sem que o saldo físico se mova.

Duas consequências grandes: o estoque fica **protegido no ERP durante a live** (o que
no Caminho B era impossível — §5.3), e o movimento manual tipo `S` **desaparece**, e com
ele a classe inteira de reservas órfãs.

#### 5.2.1 O que o Caminho A NÃO resolve

`[EMPÍRICO 26/08]` **O ERP não faz o gate.** Um pedido de 92 unidades sobre 42
disponíveis foi aceito, e o `disponivel` foi para −50. Três pedidos simultâneos
disputando a última unidade: todos reservaram. **A admissão continua sendo nossa** —
a regra de §5.4 e o portão atômico local seguem valendo integralmente.

#### 5.2.2 A troca que a migração exige: ler `disponivel`, não `saldo`

`[EMPÍRICO 26/08]` No Caminho A o **saldo físico não se move durante a live**. O webhook
de estoque continua disparando, mas carregando o `saldo` — inalterado. Um espelho que
leia o físico veria um número constante enquanto o `disponivel` despenca, e a live
ofereceria unidade que já tem dono.

O código já resolve isso: `ExtrairSaldoDisponivel` (`tiny.go:545`) lê `disponivel` e
satura negativo em zero, e o provider troca físico por disponível quando
`use_available_stock` está no metadata da integração (`factory.go:117`) — **por
integração, não por loja**, porque um lojista pode ter mais de um ERP.

`[EMPÍRICO 26/08]` Validado ponta a ponta com a flag ligada: com `saldo=9`,
`reservado=3` e `disponivel=6` no Tiny, o estoque local do LiveCart ficou em **6**.

**Portanto a migração para o Caminho A é: instalar o módulo na conta + ligar
`use_available_stock` naquela integração.** Nenhum dos dois é código novo.

#### 5.2.3 A detecção automática segue sem endpoint

`[EMPÍRICO 26/08]` `GET /depositos` continua devolvendo **403** mesmo com o módulo
instalado e com o role `depositos-leitura` no token. Então `possuiReserva` não serve
como detector nesta conta. A detecção prática é comportamental e não custa escrita:
ler `GET /estoque/{id}` e verificar se `disponivel` acompanha `saldo − reservado` em
vez de ser zero constante.

### 5.3 No Caminho B, o estoque só é lançado no pagamento

`[EMPÍRICO 11/07 T6 e 26/08]` Estoque lançado **bloqueia** o `PUT /itens`
(`400 pedido.motivosBloqueio[0]: "estoque lançado"`). Logo, no Caminho B, *pedido mutável* e
*estoque protegido no ERP durante a live* são **mutuamente exclusivos**.

Decisão: **durante a live o pedido é mutável e o estoque NÃO é lançado.** A proteção na
janela da live é o nosso gate local (§4.2), que é provadamente correto. O lançamento
acontece uma vez, no pagamento — que é também quando o ERP passa a validar saldo (§4).

Isso **elimina o movimento manual tipo `S`**, e com ele: o webhook de estoque que realimenta
a waitlist, o par estorno→criação, e a classe inteira de reservas órfãs.

### 5.4 Uma escrita por carrinho por janela, não uma por item

Com 30 escritas/minuto por conta, uma escrita por item é impossível. O `PUT /itens` manda a
**grade final**, então N adições viram **uma** escrita: coalescing por carrinho, com debounce,
uma escrita por janela.

### 5.5 Fila serial por pedido, e nunca duas escritas do mesmo pedido em voo

Exigido por §4.1. Uma fila por `idPedido`, ordem garantida, concorrência global limitada a
**4** (o balde de rajada). Nunca dois `PUT /itens`, dois `lancar-estoque` ou dois
`estornar-estoque` do mesmo pedido simultâneos.

### 5.6 O prazo deixa de matar a escrita

`[EMPÍRICO 26/08]` Os 115 `unconfirmed` foram timeouts do prazo de 90 s **na fila interna**,
não recusas do ERP. Uma escrita que nunca foi despachada é **provadamente não aplicada** e
seguramente repetível — mas hoje é arquivada como ambígua.

Decisão: **distinguir "não despachada" de "em voo"**. Só o que saiu da máquina pode virar
`unconfirmed`; o que morreu na fila vira `failed`, que re-tenta. É a mesma lição do fix
`8e633f0`, aplicada à fila em vez de ao provider.

### 5.7 Cancelar estorna contas, e só contas

`[EMPÍRICO 26/08]` `PUT /situacao = 2` **estorna as contas a receber sozinho**, e
**não** estorna o estoque lançado.

Decisão: no cancelamento, **estornar estoque explicitamente e nunca estornar contas**.
Inverter qualquer um dos dois gera estorno duplo.

### 5.8 Toda invariante de estado é nossa

`[EMPÍRICO 26/08]` Nove transições encadeadas, **todas aceitas**, inclusive `6 Entregue → 0
Aberta` e `2 Cancelada → 3 Aprovada`, com o `GET` confirmando que a situação mudou de fato.
**O ERP não impõe máquina de estados via API.**

### 5.9 O webhook não é autenticado

`[EMPÍRICO 26/08]` A primeira entrega real do painel trouxe apenas headers do Cloudflare, o
tracing interno deles e `User-Agent: Go-http-client/2.0`. Nenhuma assinatura. `[DOC]` A página
oficial de webhooks não menciona HMAC, assinatura ou token.

O `storeId` na URL é o único segredo. Decisão: **acrescentar um segredo por loja ao caminho**
e rejeitar quem não o apresentar. `[DOC]` O Tiny re-entrega **até 10 vezes, +5 min a cada
tentativa**, enquanto não receber 200 — então rejeitar é seguro, não perde evento.

---

## 6. O que muda no código

| # | Mudança | Depende de |
|---|---|---|
| 1 | Fila serial por pedido, concorrência global ≤ 4 | — |
| 2 | Coalescing por carrinho com debounce; `PUT /itens` com a grade final | 1 |
| 3 | Classificar timeout de FILA como `failed`, não `unconfirmed` | — |
| 4 | Detector `GET /depositos.possuiReserva` no onboarding, por loja | conta com o módulo |
| 5 | Pedido criado na primeira admissão, mutável, sem lançar estoque | 1, 2 |
| 6 | Pagamento: `parcelas` → `lancar-contas` → `lancar-estoque` → situação | 5 |
| 7 | Cancelamento: estornar estoque, **não** estornar contas | 5 |
| 8 | Segredo por loja no caminho do webhook | — |
| 9 | Import respeitar `situacao` (hoje traz produto `E`/excluído como ativo) | — |

Os itens **1, 3, 8 e 9 não dependem do resto** e podem subir antes da refatoração.

---

## 7. Consequências

**A favor.** O lojista passa a ter documento no ERP desde o primeiro item. Ganha-se a trava
de saldo do ERP no lançamento. Some o movimento manual, o webhook de estoque que realimenta
a waitlist e a classe de reservas órfãs. As escritas por live caem de uma-por-item para
uma-por-carrinho-por-janela.

**Contra.** Durante a live o estoque não fica protegido no ERP — outro canal pode vender a
unidade que admitimos. É uma **regressão consciente** frente ao movimento manual de hoje, e o
preço de ter o pedido mutável no Caminho B. Mitigações: a janela é curta, o gate local é
conservador, e no Caminho A (quando houver conta) a reserva do pedido elimina o problema.

**Risco aceito.** O Caminho A permanece não verificado. Se `PUT /itens` não funcionar em
pedido apenas reservado, o Caminho A cai e ficamos só com o B — o que **não invalida nada
deste ADR**, porque o B está inteiramente provado.

---

## 8. O que os testes de 26/08 fecharam, e o que sobrou

**Fechado.** O Caminho A está verificado (§5.2): o pedido reserva sem mexer no físico,
`PUT /itens` funciona e reajusta a reserva, cancelar devolve sozinho, o lançamento
converte limpo, e o espelho segue o `disponivel` com `use_available_stock` ligado.

**Sobrou.**

1. `GET /estoque/{id}/logs-movimentacao` continua **403** — o shape real do log de
   movimentação segue sem verificação, e com ele a possibilidade de resolver a classe
   `unconfirmed` por consulta determinística em vez de sonda de saldo.
2. `GET /depositos` continua **403**, então não há detector automático de conta.
3. O **rate limit no Caminho A** não foi medido sob carga de live: a reserva agora é
   `POST /pedidos` + `PUT /itens` em vez de `POST /estoque`, e o custo por carrinho muda.
4. A **migração dos pedidos legados** — carrinhos com reserva por movimento manual
   convivendo com carrinhos por reserva de pedido — não tem plano escrito nem teste.
