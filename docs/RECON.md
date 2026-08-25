# RECON — Fase 0 da refatoração da integração Tiny/Olist ERP v3

> **Entregável de descoberta. Nenhuma linha de código de produção foi escrita para produzir este documento.**
> Data de fechamento: **25/08/2026**. Base de código: `livecart-be` @ `c7d4ced` (branch `stg`).
> Swagger de referência: `Olist ERP API v3`, `info.version 3.1`, OpenAPI 3.0.0, **127 paths / 202 operações /
> 348 schemas**, `servers[0].url = https://api.tiny.com.br/public-api/v3`, **1.110.748 bytes** (1.109.750 caracteres)
> `[EMPÍRICO 25/08 — verificado com python3 sobre /mnt/c/Users/aliss/Downloads/swagger.json]`.
> O arquivo local é **bit-a-bit idêntico** ao publicado em `erp.tiny.com.br/public-api/v3/swagger/swagger.json`
> (md5 `7cd62fba1dcfe41aae0b2c52e5ec63eb`) — **o que falta aqui falta na spec oficial**.

## Convenção de procedência — vale para cada linha deste documento

| Marca | Significado |
|---|---|
| `[EMPÍRICO 25/08]` | Medido **hoje** contra a conta real ADABYTE com `apps/tiny-lab`. Evidência crua em `.tiny-lab/audit.jsonl` e `ratelimit-burst.json`. |
| `[EMPÍRICO 11/07]` | Bateria de sandbox de 11/07/2026 — 333 requests, 75 webhooks, conta ADABYTE. Cita o teste (T1..T11) e a linha do `actions.jsonl`. |
| `[EMPÍRICO prod]` | Observado no Postgres de produção, nos logs do Railway ou em corpo real transcrito em comentário de código. |
| `[SWAGGER]` | **O arquivo afirma.** Não é prova de comportamento do ERP. Onde o comportamento importa, está dito qual teste fecharia. |
| `[CÓDIGO arq:linha]` | O que o nosso Go faz hoje. |
| `[ANÁLISE]` | Inferência minha — sempre acompanhada do que a sustenta. |
| `[ABERTO]` | Não sabemos. Vem sempre com a chamada exata que responderia. |

**Regra de leitura:** `[SWAGGER]` nunca é usado como prova de comportamento. Onde a única fonte é o swagger e o
comportamento decide algo, o item aparece como `[ABERTO]` com o teste nomeado.

---

## Índice

1. [Sumário executivo](#1-sumário-executivo)
2. [A pergunta que decide a arquitetura — Caminho A vs Caminho B](#2-a-pergunta-que-decide-a-arquitetura--caminho-a-vs-caminho-b)
3. [Pedidos](#3-pedidos)
   - [3.1 `POST /pedidos` — payload e obrigatoriedade real](#31-post-pedidos--payload-e-obrigatoriedade-real)
   - [3.2 `PUT /pedidos/{id}/itens` — a grade](#32-put-pedidosiditens--a-grade)
   - [3.3 `PUT /pedidos/{id}` — o que muda depois da criação](#33-put-pedidosid--o-que-muda-depois-da-criação)
   - [3.4 Matriz situação × operação permitida](#34-matriz-situação--operação-permitida)
   - [3.5 `PUT /situacao` — enum real, transições e efeitos colaterais](#35-put-situacao--enum-real-transições-e-efeitos-colaterais)
   - [3.6 Como buscar pelo número que o lojista vê](#36-como-buscar-pelo-número-que-o-lojista-vê)
   - [3.7 Idempotência da criação — o 409](#37-idempotência-da-criação--o-409)
4. [Estoque](#4-estoque)
5. [Contas / financeiro — território virgem](#5-contas--financeiro--território-virgem)
6. [Operacional](#6-operacional)
   - [6.1 Rate limit — a medição de hoje](#61-rate-limit--a-medição-de-hoje)
   - [6.2 Catálogo de erros com corpo exato](#62-catálogo-de-erros-com-corpo-exato)
   - [6.3 Paginação, datas e dinheiro](#63-paginação-datas-e-dinheiro)
   - [6.4 Webhooks](#64-webhooks)
   - [6.5 Autenticação](#65-autenticação)
7. [Tabela de cobertura da Fase 0](#7-tabela-de-cobertura-da-fase-0)
8. [A pauta da próxima bateria](#8-a-pauta-da-próxima-bateria)
9. [Anexo — higiene e registros do dia](#9-anexo--higiene-e-registros-do-dia)
10. [Verificação — a revisão adversarial de 25/08](#verificação)

---

# 1. Sumário executivo

## O veredito de Caminho A vs Caminho B

**A pergunta está mal posta. Não é A *ou* B — é A *e* B, e a escolha é por conta, detectável em runtime.**

| Conta | `reservado` | `disponivel` | Regime | Fonte |
|---|---|---|---|---|
| **ADABYTE** (nossa conta de teste) | `0` em 92/92 GETs de 11/07 e `0` de novo hoje | `0` em 92/92, inclusive com `saldo=465` | **B** — módulo de reserva inativo | `[EMPÍRICO 11/07: actions.jsonl, 92 GETs]` + `[EMPÍRICO 25/08]` |
| **Canto da Art** (produção) | `1` | `3` = `saldo(4) − reservado(1)` | **A** — módulo ATIVO | `[EMPÍRICO prod 14/08, corpo transcrito em tiny.go:536]` |

Na ADABYTE `disponivel` é **constante zero** — não é `saldo − reservado`, é campo morto. Em produção a conta fecha.
Qualquer código que assuma um dos dois regimes está errado para metade das lojas.

O detector determinístico está **declarado no contrato** e **nunca foi chamado**: `GET /depositos` declara
`possuiReserva`, com a descrição literal *"Indica se a conta possui o módulo de reserva de estoque ativo"*
`[SWAGGER — verificado 25/08]`. Uma leitura sem parâmetros classificaria a conta — **mas isso é o que o arquivo
promete, não comportamento medido**: a chamada devolveu **403** na ADABYTE hoje (§2.3), então nem o valor nem o
shape foram vistos. `grep possuiReserva` no repo = **0 ocorrências** `[CÓDIGO: grep]`.

## O que sabemos com confiança

- **A semântica de estoque está fechada empiricamente.** `S` subtrai, `E` soma, **`B` fixa o saldo (absoluto)** —
  `465 --B10--> 10` `[EMPÍRICO 11/07: T4]`. `POST /estoque` **não tem dedup nenhuma**: dois corpos byte-idênticos →
  dois `idLancamento`, saldo somou 2× `[EMPÍRICO 11/07: T10]`. **Retry cego sempre duplica.**
- **`lancar-estoque` é atômico e guardado** (2ª vez → `400 "Estoque já lançado."`, inclusive sob concorrência no
  mesmo milissegundo, Δsaldo=1) e **`estornar-estoque` é idempotente e tolerante a órfão** (204 sempre, no-op sem
  lançamento prévio, funciona em pedido já cancelado) `[EMPÍRICO 11/07: T2, T7]`.
- **Cancelar um pedido NÃO devolve estoque lançado** `[EMPÍRICO 11/07: T7]`.
- **`PUT /itens` é replace-all sem id de linha e sem controle de concorrência** — qty=5 ∥ qty=3 → ambos 204, grade
  final 3, last-write-wins silencioso `[EMPÍRICO 11/07: T11/C2]`. É bloqueado por **estoque lançado**, não por
  situação `[EMPÍRICO 11/07: T6-b/T6-c]`.
- **O rate limit real, medido hoje: dois baldes independentes — 4 req/s de rajada e 30 req/min sustentado
  (0,5 req/s de média). Não existe `Retry-After`.** `[EMPÍRICO 25/08]` Ver §6.1 — é a seção que mais muda o
  dimensionamento do fluxo alvo.
- **Webhooks: envelope `{versao, cnpj, tipo, dados}`, 3 tipos, sem assinatura, sem sequência, entrega não garantida,
  e o `inclusao_pedido` chega ANTES da resposta HTTP do `POST /pedidos`** (mediana −23 ms em 26 pares)
  `[EMPÍRICO 11/07]`.

## O que não sabemos

- **Se `PUT /pedidos/{id}/itens` funciona num pedido apenas reservado** numa conta com módulo de reserva ativo.
  Ver §2.4. Ninguém testou. É a suposição sobre a qual todo o fluxo alvo se apoia.
- **Se `PUT /pedidos/{id}` aceita `valorFrete`/`valorDesconto`/`enderecoEntrega`.** A afirmação de que "o frete é
  imutável após a criação" é `[SWAGGER]` puro, e **o swagger nunca fecha objeto** — `additionalProperties` aparece
  **0 vezes em 1,1 MB** `[EMPÍRICO 25/08: contagem]`. Em toda a história do projeto o `PUT /pedidos` foi chamado
  **2 vezes**, ambas só com `{"pagamento":{"parcelas":[…]}}`. A hipótese nunca foi testada; só nunca foi tentada.
- **O corpo real de `GET /estoque/{id}/logs-movimentacao`.** O endpoint existe `[SWAGGER]`, mas a resposta 200 aponta
  para `PaginatedResultModel`, que resolve para `{limit, offset, total}` **sem array de itens** `[EMPÍRICO 25/08:
  verificado]`, e o schema do item é órfão. **A conta ADABYTE responde 403 e não pode decidir isto** (ver abaixo).
- **Todo o financeiro.** `grep "lancar-contas\|estornar-contas\|contas-receber" --include=*.go` = **0 linhas**.
  Território virgem.
- **88 das 90 células da matriz de transições de `situacao`.** Só `0→3`, `0→2` e `3→2` foram exercitados, e
  **nunca houve um único 400 num `PUT /situacao`** `[EMPÍRICO 11/07]`.

## A única coisa que bloqueia o ADR

**Uma conta Tiny com o módulo de reserva ativo (`possuiReserva=true`) e com permissão de `/depositos` e
`/estoque/{id}/logs-movimentacao`.** A ADABYTE não serve para isso e isso foi provado hoje:

```
[EMPÍRICO 25/08]  200 /info                    200 /produtos?limit=1
                  200 /estoque/357281337       200 /contatos?limit=1
                  200 /pedidos?limit=1         200 /formas-pagamento
                  200 /contas-receber?limit=1
                  403 /depositos
                  403 /estoque/357281337/logs-movimentacao
```

Os dois 403 **não são falta de permissão do app**: o JWT carrega 48 roles em `resource_access.tiny-api`, incluindo
`depositos-leitura` e `estoque-leitura`. O bloqueio é de **conta/plano** `[EMPÍRICO 25/08]`. Consequência direta:
esta conta **não pode responder** (a) `possuiReserva` nem (b) o shape de `logs-movimentacao` — que são,
respectivamente, o teste nº 1 e o teste nº 2 da pauta.

**Não há sandbox do lado do fornecedor.** Host único `https://api.tiny.com.br/public-api/v3`
`[EMPÍRICO 25/08: README do tiny-lab + a própria spec, `servers` com um elemento]`. **Toda escrita da Fase 1
acontece numa conta Tiny real.** Por isso o `tiny-lab` tem guard duplo (`TINY_ENV=sandbox` + `cpfCnpj` do
`GET /info` conferido contra a allowlist `TINY_ALLOWED_CNPJ` a cada execução). **A allowlist está vazia — nenhuma
escrita foi feita nesta sessão, só GETs e a troca de token** `[EMPÍRICO 25/08]`.

## Um saldo operacional que precisa de dono

**Duas linhas do razão nunca resolvidas, em dois carrinhos pagos — consultadas por mim hoje no Postgres de
produção** `[EMPÍRICO prod 25/08]`:

| short_id | cart_status | payment_status | movimento | desde | erro |
|---|---|---|---|---|---|
| **1115** | checkout | **paid** | `unconfirmed` | 25/08 13:52 UTC | `reverse stock reservation failed: status 429` |
| **1171** | checkout | **paid** | `pending`, attempts=0 | 24/08 19:43 UTC | (nunca tentou) |

Totais do razão em produção: `confirmed/in` 477 · `confirmed/out` 587 · `pending/in` 1 · `unconfirmed/in` 1.

⚠️ **Precisão que o AUDIT §5.2 acrescenta e que este resumo não pode achatar:** as duas linhas acima são do
**razão**, e só uma delas trava um carrinho. O **#1115** está de fato travado — `erp_finalisation_status='failed'`,
3 tentativas, barrado pelo próprio gate. O **#1171** tem `erp_finalisation_status='done'` desde 24/08 19:44: a
finalização concluiu e o que ficou para trás foi a linha do razão, órfã. É vazamento de **observabilidade**, não de
estoque. E as **finalizações de carrinho pago que estão falhas são três, não duas** — `#1186`, `#1087` e `#1115`
(AUDIT §5.3), com três causas distintas.

**O defeito do #1115 já foi corrigido hoje** — commit `8e633f0` na branch `fix/erp-estorno-429-provado-nao-aplicado`,
já em origin. Causa: `ReserveStock` juntava `providers.ErrProvenUndelivered` em falha de discagem e em 4xx, mas
`ReverseStockReservation` usava `DoRequest` cru e **nunca juntava** — então todo erro de estorno virava
`unconfirmed`, que por desenho nunca re-tenta e trava a finalização. O fix tornou o estorno simétrico à reserva;
5 testes novos em `estorno_simetria_test.go`. **O fix corrige o comportamento FUTURO; as duas linhas do razão já paradas
(#1115 `unconfirmed`, #1171 `pending`) e as três finalizações falhas (#1186, #1087, #1115) continuam como estão e
precisam de ação operacional — ainda não tomada.**

---

# 2. A pergunta que decide a arquitetura — Caminho A vs Caminho B

> **Caminho A** — o Tiny tem módulo de reserva nativo (`reservado`/`disponivel` reais) e dá para reservar sem
> mexer no saldo físico.
> **Caminho B** — não tem; "reserva" = lançamento manual de saída (`POST /estoque` `tipo:"S"`), que **baixa o
> saldo físico do lojista**.

## 2.1 A evidência dos dois lados

**Lado B — ADABYTE.** Corpo literal do primeiro GET da bateria (`actions.jsonl#5`) `[EMPÍRICO 11/07]`:

```json
{"id":357281337,"nome":"Console PlayStation® 5 Slim Edição Digital 825 GB Branco - Sony",
 "codigo":"","unidade":"UN","saldo":10,"reservado":0,"disponivel":0,"localizacao":"",
 "depositos":[{"id":334779581,"nome":"Geral","desconsiderar":false,
               "saldo":10,"reservado":0,"disponivel":0,"empresa":"59573950000158"}]}
```

Tuplas `(saldo, reservado, disponivel)` distintas observadas — **todas as 92** `[EMPÍRICO 11/07]`:

```
(-1,0,0) (0,0,0) (1,0,0) (2,0,0) (3,0,0) (5,0,0) (6,0,0) (7,0,0)
(8,0,0) (10,0,0) (11,0,0) (13,0,0) (16,0,0) (465,0,0)
```

Com `saldo=465` e `reservado=0`, `disponivel` deveria ser 465. Veio **0**. O campo existe no JSON e é constante zero.

E o mesmo produto **hoje**, 45 dias depois `[EMPÍRICO 25/08]`:

```json
{"id":357281337,"nome":"Console PlayStation® 5 Slim Edição Digital 825 GB Branco - Sony",
 "codigo":"","unidade":"UN","saldo":1,"reservado":0,"disponivel":0,"localizacao":"",
 "depositos":[{"id":334779581,"nome":"Geral","desconsiderar":false,
               "saldo":1,"reservado":0,"disponivel":0,"empresa":"59573950000158"}]}
```

O saldo mudou (10 → 1) e o par `reservado/disponivel` **continua zero**. Não é estado congelado: é regime de conta.

**Lado A — produção.** Corpo real de 14/08, preservado em comentário de código `[EMPÍRICO prod]`:

```json
{"id":830590845,"nome":"Carrossel Musical Azul - 17cm","codigo":"3583A",
 "unidade":"UN","saldo":4,"reservado":1,"disponivel":3,"depositos":[…]}
```

Aqui `disponivel = saldo − reservado` fecha exatamente.

## 2.2 De onde vem o `reservado` de produção — e não somos nós

As nossas "reservas" são saídas manuais `tipo:"S"`, que descontam do **`saldo`** e **nunca aparecem em
`reservado`**: 92 GETs com `reservado=0` apesar de dezenas de saídas `S` nossas `[EMPÍRICO 11/07]`, mais o print
da loja registrado em `PLANO_RESERVA_TINY.md:81-85` com *"5 reservas LiveCart ativas e total reservado 0,00"*
`[EMPÍRICO prod]`. **Logo o `reservado=1` do Carrossel veio de um pedido/orçamento da própria Tiny, não de nós.**

E **não existe nenhum path com "reserva" na API** — nenhuma operação de criar, consultar ou liberar reserva nativa
`[SWAGGER]`. Se `reservado` sobe, é efeito colateral de um documento do ERP, não de uma chamada nossa.

## 2.3 A escolha é POR CONTA, e é detectável em runtime

`GET /depositos` — **sem nenhum parâmetro**, resposta **declarada** como array cru na raiz (não
`{itens, paginacao}`) apontando para `DetalheDepositoResponseModel` `[SWAGGER — verificado hoje; nenhum corpo real
foi visto, a conta dá 403]`:

```json
{ "id": 0,
  "descricao": "string",          // "Nome ou descrição do depósito"
  "tipo": "P",                    // "Tipo do depósito. P = Próprio, E = Exclusivo para canal de venda"
  "desconsideraSaldo": false,     // "…desconsidera o saldo no total de estoque do produto"
  "padrao": true,                 // "Indica se este é o depósito padrão"
  "possuiReserva": false }        // "Indica se a CONTA possui o módulo de reserva de estoque ativo"
```

Três fatos verificados hoje sobre esse campo `[EMPÍRICO 25/08]`:
1. A string `possuiReserva` aparece **1 vez** em todo o arquivo — a própria definição. **`GET /depositos` (array) e
   `GET /depositos/{idDeposito}` (detalhe) apontam para o MESMO model.** Ou seja, **uma leitura sem parâmetros
   classifica a conta.**
2. A descrição fala da **conta**, não do depósito, apesar de o campo viver dentro do objeto depósito — `[ANÁLISE]`
   é um flag global replicado em cada item.
3. `grep possuiReserva` no **código** = **0 ocorrências** `[CÓDIGO: grep]`. Nunca foi lido em runtime.
   ⚠️ **Mas o campo não é novidade para o acervo:** `PLANO_RESERVA_TINY.md:82` (18/08) já o citava — e o
   **descartou** com *"`possuiReserva` é só um flag do depósito"*. **É exatamente essa leitura que o item 2 acima
   desfaz:** a descrição do próprio schema fala da **conta**. O campo não estava desconhecido; estava mal lido, e
   por isso o detector foi jogado fora antes de ser chamado.

**Conclusão de arquitetura:** o cliente ERP precisa de um **gate de onboarding por loja**, resolvido com uma
chamada a `GET /depositos` no momento do connect e persistido na integração. Qualquer escolha global entre A e B
estará errada para metade das lojas. Projetar para o **híbrido**.

`[ABERTO]` `GET /depositos` **não pôde ser exercitado**: a conta ADABYTE devolve **403** `[EMPÍRICO 25/08]`, e o
403 é de conta/plano, não de escopo do app (o JWT tem `depositos-leitura` entre as 48 roles). **Chamada que
fecharia:** `GET /depositos` numa conta com o módulo ativo — leitura pura, sem risco, e serve para a Canto da Art.

## 2.4 🔴 O que NÃO foi provado — e todo o fluxo alvo depende disso

**Ninguém nunca testou se `PUT /pedidos/{id}/itens` funciona num pedido apenas RESERVADO.**

O que temos hoje é isto, e só isto `[EMPÍRICO 11/07: T6]`:

| Estado do pedido | `PUT /itens` | Fonte |
|---|---|---|
| Aberta (`situacao=0`), **sem** estoque lançado | **204** | T6-a |
| **Aprovada** (`situacao=3`), sem estoque lançado | **204** | T6-c |
| Aberta, **com estoque lançado** | **400** `motivosBloqueio: "estoque lançado"` | T6-b |

Os três casos rodaram numa conta **Caminho B**, onde **não existe estado "reservado"** — o pedido ou tem estoque
lançado, ou não tem. Numa conta **Caminho A**, o pedido pode existir num terceiro estado: **com reserva nativa
ativa e sem baixa efetivada**. `[ABERTO]` — os três pontos abaixo não têm nenhuma medição, nem nossa nem de
terceiros, e a sequência de 10 chamadas no fim desta seção é o que fecha os três:

- Se `PUT /itens` é aceito nesse estado, ou se cai no mesmo `motivosBloqueio`;
- Se a reserva nativa **acompanha** a nova grade (como o `lancar-estoque` acompanhou, em T6-b-ciclo), ou se fica
  dessincronizada;
- **Em que momento** a reserva nativa nasce — no `POST /pedidos`? na aprovação (`situacao=3`)? só no
  `lancar-estoque`?

**É a suposição sobre a qual todo o fluxo alvo se apoia — "o pedido é a reserva, e a grade é mutável enquanto o
carrinho vive" — e ninguém a testou.** Se `PUT /itens` for bloqueado num pedido reservado, o desenho "pedido
mutável" morre e sobra `cancelar + recriar` a cada mudança de carrinho, o que é inviável no orçamento de chamadas
de uma live (§6.1).

**Chamadas que fechariam, em ordem, numa conta com `possuiReserva=true`:**

```
1. GET  /depositos                                   → possuiReserva? padrao? quantos?
2. GET  /estoque/{idProduto}                         → (saldo, reservado, disponivel) BASE
3. POST /pedidos {idContato, data, itens:[…]}        → id
4. GET  /estoque/{idProduto}                         → reservado subiu no POST?
5. PUT  /pedidos/{id}/situacao {"situacao":3}
6. GET  /estoque/{idProduto}                         → reservado subiu na aprovação?
7. PUT  /pedidos/{id}/itens {"itens":[…grade nova…]} → 204 ou 400 motivosBloqueio?   ← A PERGUNTA
8. GET  /estoque/{idProduto}                         → a reserva acompanhou a grade?
9. POST /pedidos/{id}/lancar-estoque
10. GET /estoque/{idProduto}                         → reservado→saldo, ou soma?
```

Dez chamadas. **Cabem em 20 segundos** dentro do balde sustentado de 30 req/min (§6.1) e decidem o ADR inteiro.

## 2.5 O terceiro caminho: `logs-movimentacao`

Independente de A vs B, uma premissa que sustenta metade do desenho atual precisa ser reaberta.

`[CÓDIGO tiny.go:2208-2211 / movement_ledger.go:9-10]` — comentário literal: *"a API do Tiny não oferece consulta
de lançamentos (só criar e estornar), então não há como perguntar depois 'chegou?'"*. **É essa frase que faz um
timeout virar `unconfirmed` e travar a finalização de um carrinho pago.** `PLANO_RESERVA_TINY.md:79-87` (18/08) vai
além: *"Não existe listagem de lançamentos/movimentações na API pública v3"* → *"**Veredito: Estratégia A está
morta.**"* — e a mesma página abre com *"A tag Estoque tem exatamente DOIS endpoints"*, que é a premissa falsa.

`[SWAGGER]` A tag Estoque tem **TRÊS** operações, não duas:

```
GET  /estoque/{idProduto}
POST /estoque/{idProduto}
GET  /estoque/{idProduto}/logs-movimentacao   ← operationId: ListarLogsMovimentacaoEstoqueAction
     summary: "Listar logs de movimentação de estoque (autoria e origem)"
     query: dataInicio (Y-m-d) · dataFim (Y-m-d) · tipo (E|S|B) · idDeposito · limit(100) · offset(0)
```

O schema do item traz exatamente o que precisaríamos — detalhado em §4.3. **Mas nada disso é comportamento
comprovado:** o endpoint devolveu **403 na ADABYTE hoje** `[EMPÍRICO 25/08]`, e a resposta 200 do swagger está
anotada sem array de itens.

**Veredito honesto: a Estratégia A não caiu — está SUSPENSA até uma chamada real.** A premissa factual que a matou
(*"não existe listagem"*) está errada; a premissa que a substituiria (*"e a listagem devolve a observação que
enviamos"*) ainda não tem prova.

---

# 3. Pedidos

## 3.1 `POST /pedidos` — payload e obrigatoriedade real

### O que o swagger diz vs. o que é obrigatório de verdade

`[EMPÍRICO 25/08: varredura programática de toda a cadeia allOf/$ref de CriarPedidoModelRequest]` — arrays
`required`: **ZERO**. Nem `idContato`, nem `itens`, nem `data`. Para contraste, no **mesmo arquivo**
`CriarContaReceberRequestModel` declara `required: ["dataVencimento","valor","contato"]` e
`AtualizarSituacaoPedidoModelRequest` declara `required: ["situacao"]` — **a mecânica existe e simplesmente não foi
aplicada ao POST de pedidos**. O único sinal em todo o corpo é `ProdutoRequestModel.id` com `"nullable": false`.

Reforço que fecha a questão `[EMPÍRICO 25/08: contagens sobre o arquivo]`:

```
additionalProperties : 0 ocorrências     maxLength : 0
readOnly             : 0                 minLength : 0
writeOnly            : 0                 pattern   : 0
deprecated           : 0                 429 / RateLimit / Retry-After / Idempotency : 0
arrays "required"    : 24 em 348 schemas
                       45 dos 68 schemas de request DISTINTOS não têm nenhum
                       (= 65 das 89 operações com requestBody)
```

**Nenhum schema fecha o objeto.** O swagger nunca diz "só estes campos"; diz "pelo menos estes campos". Isso tem
duas consequências opostas e igualmente importantes:

- **Parsers de resposta podem ser gerados do swagger.** `[EMPÍRICO 25/08: diff programático]` os schemas
  `ObterPedidoModelResponse` e `ListagemPedidoModelResponse` batem **campo a campo** com os corpos reais de 11/07:
  `swagger-only: []`, `real-only: []`, nos dois. **Este swagger é confiável na resposta.**
- **Validação de request NÃO pode ser derivada dele.** Cada campo de escrita precisa de teste próprio.

### Mínimo que funciona na prática

`[EMPÍRICO 11/07: actions.jsonl#9]` — request e response literais:

```json
POST /pedidos
{"data":"2026-07-11","idContato":895591553,
 "itens":[{"produto":{"id":357281337},"quantidade":2,"valorUnitario":10}],
 "observacoes":"LIVECART SANDBOX — pedido de teste, pode cancelar",
 "ecommerce":{"numeroPedidoEcommerce":"LCSBX-T1-0711-1437"}}

→ 201  {"id":358126298,"numeroPedido":"1"}
```

⚠️ **A resposta é 201, e o swagger declara só 200/204.** `[EMPÍRICO 11/07]` `providers.IsSuccessStatus` cobre, então
não é bug hoje — mas é o **único** desvio de resposta encontrado em todo o recon, e qualquer gerador estrito quebra.

⚠️ **`numeroPedido` volta como STRING no POST e como INTEIRO no `GET /pedidos/{id}`** (`"numeroPedido":"1"` vs
`"numeroPedido": 1`) `[EMPÍRICO 11/07: #9 vs #10]`. O mesmo vale para `valor` na listagem (string `"10"`) contra
`valorTotalPedido` no detalhe (float). **Isto não é bug de anotação do swagger — o swagger está certo e a API é
assim mesmo.** Um cliente Go precisa de dois tipos distintos; não espere correção do fornecedor.

⚠️ `CriarPedidoModelResponse` **não tem `situacao`** `[SWAGGER]` — e `[CÓDIGO tiny.go:1451-1455]` parseia e loga um
campo que nunca vem.

### Estrutura completa do corpo `[SWAGGER]`

Escalares de topo: `data` · `dataPrevista` · `dataEnvio` (`example` **com hora**: `"2024-01-01 00:00:00"`) ·
`dataEntrega` · `numeroOrdemCompra` · `observacoes` · `observacoesInternas` · `situacao` (enum completo) ·
`valorDesconto` · `valorFrete` · `valorOutrasDespesas` (todos float) · `idContato` (**integer escalar**, não objeto).

Objetos: `listaPreco{id}` · `naturezaOperacao{id}` · `vendedor{id}` · `enderecoEntrega{}` ·
`consumidorFinal{cpfCnpj, clienteConsumidorFinal}` · `ecommerce{id, numeroPedidoEcommerce}` · `transportador{}` ·
`intermediador{id}` · `deposito{id}` · `pagamento{}` · `itens[]` · `pagamentosIntegrados[]`.

**Não existe `marcadores` no corpo do POST** — varredura case-insensitive por "marcador" no corpo resolvido = 0.
Marcador só entra depois, por `POST /pedidos/{id}/marcadores`.

**`itens[]` (`ItemPedidoRequestModel`) — a única grade que existe:**
```
produto : { id: integer  ← "nullable": false, ÚNICO campo não-nullable do corpo inteiro
            tipo: "P"|"S" (Produto|Serviço), nullable }
quantidade    : float | null
valorUnitario : float | null
infoAdicional : string | null
```
**Não existe `sku`, `codigo`, `descricao` nem id de linha no request de item.** `sku` só aparece na *resposta*.
Todo mapeamento passa pelo `produto.id` interno do Tiny — o que obriga um cache local de catálogo, já que também
não existe `GET /produtos/por-sku` nem busca em lote (§6.3).

**`enderecoEntrega`** `[SWAGGER]`: `endereco`, **`enderecoNro`**, `complemento`, `bairro`, `municipio`, `cep`, `uf`,
**`fone`**, `nomeDestinatario`, `cpfCnpj`, **`ie`**, `tipoPessoa` (enum `J|F|E|X`).
⚠️ **Assimetria request/response**: o response usa `numero`/`telefone`/`inscricaoEstadual` e ainda expõe `pais`,
que o request não aceita. **Um round-trip GET→PUT não é simétrico.**

**`transportador`** `[SWAGGER]`: `id` · `fretePorConta` enum `["R","D","T","3","4","S"]`
(R Remetente · D Destinatário · T Terceiros · 3 Próprio Remetente · 4 Próprio Destinatário · **S Sem Transporte**) ·
`formaEnvio{id}` · `formaFrete{id}` · `codigoRastreamento` · `urlRastreamento`.

**`pagamentosIntegrados[]`** `[SWAGGER]`: `valor` · `tipoPagamento` (integer, **sem enum e sem tabela de domínio no
arquivo**) · `cnpjIntermediador` · `codigoAutorizacao` · `codigoBandeira` (integer, idem). `[ABERTO]` a tabela de
códigos fiscais não é obtenível pela API.

### O que já custou dinheiro e não está no swagger

| Regra | Evidência |
|---|---|
| **`formaEnvio.id` não habilitada rejeita o PEDIDO INTEIRO** com `transportador.formaEnvio.id: "Forma de envio não habilitada"` — uma venda Pix de R$ 4,90 morreu assim em 16/08 | `[EMPÍRICO prod]`; workaround = **segundo POST** sem `formaEnvio` `[CÓDIGO tiny.go:1427-1442]` |
| **`meioPagamento.id` NÃO é o id de `/formas-pagamento`** — enviar o do cadastro dá `pagamento.parcelas[0].meioPagamento.id: "Meio de pagamento não encontrado"`. Foi desativado na criação | `[CÓDIGO tiny.go:1300-1310]` |
| **O Tiny valida a `formaRecebimento` da PARCELA contra a do PEDIDO** e rejeita divergência com `400 "Forma de recebimento da parcela diferente da forma de recebimento do pedido"` — e o `PUT /pedidos` não deixa alinhar a do pedido | `[EMPÍRICO 11/07: E2E]` + `[CÓDIGO tiny.go:2127-2136]` |
| O Tiny **enriquece** o pedido sozinho: `deposito {334779581,"Geral"}`, `naturezaOperacao {334779520}`, `listaPreco {0,"Padrão"}` e **`formaPagamento`/`formaRecebimento` = id 1, "Múltiplas"** — nada disso foi enviado | `[EMPÍRICO 11/07: GET dos 42 pedidos]` |

`[ANÁLISE]` **O default id=1 "Múltiplas" é candidato direto a explicar o 400 de forma de recebimento da parcela**:
a parcela chega com Pix e o pedido está em "Múltiplas". Ninguém ligou os dois. **Teste que fecha:** criar pedido
enviando `pagamento.formaRecebimento` explicitamente e ver se a validação para de reclamar — se sim, a solução é
no POST e o `PUT /pedidos` deixa de ser necessário para isso.

### Perguntas abertas do POST

| # | Pergunta | Status | Chamada que fecharia |
|---|---|---|---|
| P1 | Quais campos o Tiny **de fato** exige? | `[ABERTO]` | `POST /pedidos {}` → catalogar `detalhes[].campo`; depois remover um a um `idContato`, `data`, `itens`, `itens[].quantidade`, `itens[].valorUnitario` |
| P2 | **`situacao: 3` no POST cria já Aprovada?** | `[ABERTO]` — `[SWAGGER]` o enum está no corpo; `[EMPÍRICO 11/07]` **os 42 POSTs usaram exatamente 6 chaves** (`data` 42×, `idContato` 42×, `itens` 42×, `observacoes` 42×, `numeroOrdemCompra` 20×, `ecommerce` 4×). **`situacao` nunca foi enviada** | `POST /pedidos {…,"situacao":3}` → `GET /pedidos/{id}` + `GET /estoque/{id}` |
| P3 | Endereço + frete no payload funcionam? | `[ABERTO]` — **nenhum dos 42 pedidos da bateria teve `enderecoEntrega`, `transportador` ou `valorFrete`**; todos `null`/0 nos GETs. **É a parte do payload que mais falha em produção** | criar 1 pedido com o bloco completo e ler o GET |
| P4 | `valorFrete` entra em `valorTotalPedido`? | `[ABERTO]` — `valorTotalPedido` **não tem `description`**. A fórmula *"itens + extras − desconto + frete + impostos"* existe **só para orçamentos** (`ObterOrcamentoModelResponse.valorTotal`) `[SWAGGER]` | criar pedido com frete e comparar `valorTotalPedido` vs `valorTotalProdutos` |
| P5 | `fretePorConta:"S"` serve para retirada? | `[ABERTO]` — `[CÓDIGO tiny.go:1209]` fixamos `"D"` sempre e só omitimos `formaEnvio` na retirada | `POST /pedidos` com `fretePorConta:"S"` e sem `formaEnvio`, com `valorFrete>0` e com 0 |
| P6 | `data` aceita ISO com hora/timezone? | `[ABERTO]` | POST com `"2026-08-25T14:00:00Z"` e com `"2026-08-25"` |

## 3.2 `PUT /pedidos/{id}/itens` — a grade

### Substitui a grade? O contrato diz isso duas vezes — e há uma prova empírica

`[SWAGGER]` — a **única operação da tag com descrição funcional completa**, citada literal:

> **"Envie a grade de itens como ela deve ficar após salvar: altere quantidade, valor unitário, produto ou monte a
> lista final com inclusões e exclusões. O corpo substitui os itens atuais; totais, impostos e valores das parcelas
> existentes são recalculados."**

E no campo `AtualizarPedidoItensModelRequest.itens`:
> **"Grade final de itens do pedido (substitui a lista atual). Inclua todas as linhas desejadas após a alteração."**

Shape (mesmo `ItemPedidoRequestModel` do POST), resposta **204 No Content**:

```json
{"itens":[{"produto":{"id":834962410,"tipo":"P"},"quantidade":3,
           "valorUnitario":89.9,"infoAdicional":"texto livre"}]}
```

`[EMPÍRICO 11/07]` enviamos só `{"produto":{"id":N},"quantidade":Q,"valorUnitario":V}` — sem `tipo`, sem
`infoAdicional` — e é aceito.

⚠️ **A frase acima é `[SWAGGER]`, não comportamento medido.** O que sustenta "substitui" empiricamente é **um**
caso: `[EMPÍRICO 11/07: T6-e]` `PUT /itens` com `{"itens": []}` → **204**, e a varredura seguinte mostra o pedido
`358128735` sem itens e com `"valor": ""`. **Num merge, o array vazio seria no-op** — logo o corpo substitui.
`[ABERTO]` o que **não** está provado é a remoção seletiva: nunca se enviou uma grade com **menos linhas** que a
atual e se conferiu que as omitidas sumiram. **Chamada que fecharia:** pedido com 2 linhas → `PUT /itens` com 1 →
`GET /pedidos/{id}`.

### 🔴 Não existe id de linha em lugar nenhum da API

Nem no request (`ItemPedidoRequestModel`), nem no response (`ItemPedidoResponseModel`). Sem `id`, sem índice, sem
`sku` aceito na entrada `[SWAGGER]`. Consequências:

1. **Toda mutação é read-modify-write da grade inteira**: `GET /pedidos/{id}` → reconstruir em memória → `PUT`.
2. **Não há ETag, `If-Match`, `version` nem `updated_at`** no schema do pedido. E `[EMPÍRICO 11/07: T11/C2]`
   qty=5 ∥ qty=3 → **ambos 204, grade final 3**. Last-write-wins puro, sem lock, sem 409. **Uma edição some em
   silêncio.** Serialização por pedido tem de ser nossa.
3. Duas linhas com o **mesmo `produto.id`**: `[ABERTO]` — aceita duplicata, mescla somando, ou 400?

### O que acontece com desconto / frete / parcelas / impostos

| Item | O que se sabe | Marca |
|---|---|---|
| **Totais** | "totais … são recalculados" | `[SWAGGER]` |
| **Impostos** | "impostos … são recalculados" | `[SWAGGER]` |
| **Parcelas** | "valores das parcelas **existentes** são recalculados". Comprovado num caso: com parcela de 20, `PUT /itens` reduzindo a grade → o `GET` seguinte mostra `parcelas[0].valor = 10` | `[SWAGGER]` + `[EMPÍRICO 11/07: T6-d]` **com ressalva honesta: não houve GET intermediário provando que a parcela chegou a valer 20; o que está provado é que o PUT pediu 20 e o GET pós-`/itens` devolveu 10** |
| Com **3 parcelas** e o total mudando, como distribui? Com **0 parcelas**, cria alguma? | `[ABERTO]` | — |
| **`valorFrete`** | **NADA.** O campo não existe neste corpo e a descrição não o cita | `[SWAGGER]` → `[ABERTO]` |
| **`valorDesconto`** | idem | `[SWAGGER]` → `[ABERTO]` |
| **`valorOutrasDespesas`** | idem | `[SWAGGER]` → `[ABERTO]` |

⚠️ **Este é o `[ABERTO]` que decide "pedido mutável" vs "cancelar e recriar".** Se o `PUT /itens` zerar o frete,
toda mutação de carrinho com frete cotado exige recriar o pedido — e o frete só entra na criação (§3.3).
**Chamada que fecharia:** criar pedido com `valorFrete: 15.90` e `valorDesconto: 5` → `PUT /itens` → `GET` e ler
os dois campos.

### Item removido some limpo?

`[EMPÍRICO 11/07: T6-e]` `PUT /itens` com `{"itens": []}` → **204**. Aceita esvaziar a grade; a varredura seguinte
(`#206`) mostra o pedido `358128735` com `"valor": ""` na listagem — **pedido válido, sem itens e sem valor**.
`[ANÁLISE]` some limpo do ponto de vista da grade, mas deixa um pedido-fantasma de valor vazio no ERP do lojista —
não é um estado que a gente queira produzir em volume.

### O bloqueio real: estoque lançado, não situação

`[EMPÍRICO 11/07: T6]`

| Caso | HTTP | Efeito |
|---|---|---|
| T6-a — Aberta, **sem** estoque lançado, qty 2→3 | **204** | grade muda; saldo intacto (5→5) |
| T6-b — Aberta, **com** estoque lançado, qty 2→1 | **400** | grade travada, estoque **não** se auto-movimenta |
| T6-b ciclo — `estornar-estoque` → `PUT /itens` qty 3 → `lancar-estoque` | 204/204/204 | saldo 3→5→5→**2**: o lançamento **respeita a grade nova** |
| T6-c — pedido **APROVADO** (`situacao=3`), sem lançamento, qty 1→2 | **204** | aceito |

Corpo exato do 400 `[EMPÍRICO 11/07: 2 ocorrências, actions.jsonl]`:

```json
{"mensagem":"Ocorreram erros de validação",
 "detalhes":[{"campo":"pedido.motivosBloqueio[0]","mensagem":"estoque lançado"}]}
```

⚠️ **`motivosBloqueio` NÃO existe no `ErrorDTO` do swagger.** É um terceiro shape de erro, com `campo` em caminho
**indexado** (`[0]`) — pode vir mais de um motivo. Qualquer parser tem de tratar array.

**O ciclo obrigatório para editar um pedido já lançado é `estornar-estoque` → `PUT /itens` → `lancar-estoque`** —
três chamadas sequenciais, com a janela entre elas **sem proteção alguma do lado do Tiny**.

## 3.3 `PUT /pedidos/{id}` — o que muda depois da criação

`AtualizarPedidoModelRequest` **declara** sete coisas e só `[SWAGGER]` — declarar não é o mesmo que aceitar; ver o
bloco seguinte:

```
dataPrevista · dataEnvio · observacoes · observacoesInternas
pagamento.parcelas[] · pagamento.categoria · pagamentosIntegrados[]
```

**Não declara**: `itens`, `situacao`, `data`, `dataEntrega`, `idContato`, `enderecoEntrega`, `transportador`,
`valorFrete`, `valorDesconto`, `valorOutrasDespesas`, `numeroOrdemCompra`, `ecommerce`, `vendedor`, `deposito`,
`naturezaOperacao`, `listaPreco`, `intermediador`, `consumidorFinal`, `pagamento.formaRecebimento`,
`pagamento.meioPagamento`.

### 🔴 A afirmação "frete é imutável" é inferência, não fato

Três relatórios de recon escreveram *"o frete de um pedido é imutável após a criação"* apoiados **só** no swagger.
Isso é **inferir proibição a partir de ausência**, num documento onde `additionalProperties` aparece **0 vezes** —
ou seja, **nenhum schema fecha o objeto** `[EMPÍRICO 25/08: contagem]`. E `[EMPÍRICO 11/07]` o `PUT /pedidos` foi
chamado **2 vezes em toda a história do projeto**, ambas com **só** `{"pagamento":{"parcelas":[…]}}`.
**Nunca se tentou enviar `valorFrete`, `valorDesconto`, `enderecoEntrega` ou `itens` num PUT.**

**Status: `[ABERTO]`.** Chamada que fecharia — quatro PUTs num pedido descartável, lendo o GET depois de cada um:
```
PUT /pedidos/{id} {"valorFrete": 15.9}
PUT /pedidos/{id} {"valorDesconto": 5}
PUT /pedidos/{id} {"enderecoEntrega": {…}}
PUT /pedidos/{id} {"itens": [...]}
```

### Semântica: é PATCH real, e é o OPOSTO do `PUT /itens`

`[EMPÍRICO 11/07: T5]` Enviado `{"pagamento":{"parcelas":[{"data":"2026-07-11","dias":0,"valor":10}]}}` → **204**.
`observacoes` e `numeroOrdemCompra`, omitidos do corpo, **sobreviveram intactos**.

⚠️ **Dois verbos PUT no mesmo recurso com semânticas opostas**: `PUT /pedidos/{id}` faz **merge**;
`PUT /pedidos/{id}/itens` faz **replace declarado**.

E o round-trip **não é fiel** — o Tiny grava coisas que não enviamos `[EMPÍRICO 11/07: T5, GET antes vs depois]`:

```json
// antes: "condicaoPagamento": null, "parcelas": null
// depois:
"pagamento":{"formaPagamento":{"id":1,"nome":"Múltiplas"},
 "formaRecebimento":{"id":1,"nome":"Múltiplas"},"meioPagamento":null,
 "condicaoPagamento":"0",                                     ← STRING, não enviada
 "parcelas":[{"dias":0,"data":"2026-07-11 00:00:00",          ← data virou Y-m-d H:i:s
   "valor":10,"observacoes":"",
   "formaPagamento":{"id":9,"nome":""},                       ← id 9, não enviado
   "formaRecebimento":{"id":9,"nome":""},"meioPagamento":null}]}
```

`condicaoPagamento` **só existe no response** — não há como escrevê-lo `[SWAGGER]`.

### O que existe para frete depois da criação

`PUT /pedidos/{id}/despacho` `[SWAGGER]`, 204, nenhum campo required: `codigoRastreamento`, `urlRastreamento`,
`formaEnvio{id}`, `formaFrete{id}`, **`fretePagoEmpresa`**, `dataPrevista`, `idContatoTransportadora`, `volumes`,
`pesoBruto`, `pesoLiquido`, `observacoes`.

⚠️ **`fretePagoEmpresa` é o custo pago pela empresa, NÃO é o `valorFrete` cobrado do cliente.** Não serve para
corrigir o frete da venda. `[ABERTO]` se o despacho é merge ou replace, e se muda `situacao` para Enviada.

## 3.4 Matriz situação × operação permitida

Legenda: ✅ comprovado · ❌ comprovado que bloqueia · ❔ nunca testado · `—` não se aplica.

| Situação do pedido | `PUT /itens` | `PUT /situacao` | `lancar-estoque` | `estornar-estoque` | `PUT /pedidos` | `POST /marcadores` |
|---|---|---|---|---|---|---|
| **0 Aberta**, sem estoque lançado | ✅ 204 `[11/07 T6-a]` | ✅ →3 e →2 `[T7]` | ✅ 204 `[T1]` | ✅ 204 no-op `[T2]` | ✅ 204 `[T5]` | ✅ 204 `[T8]` |
| **0 Aberta**, **com estoque lançado** | ❌ **400** `motivosBloqueio` `[T6-b]` | ✅ →2 204 (e **não devolve estoque**) `[T7]` | ❌ **400** "Estoque já lançado." `[T2]` | ✅ 204, devolve `[T2]` | ❔ | ❔ |
| **3 Aprovada**, sem lançamento | ✅ 204 `[T6-c]` | ✅ →2 204 `[T7]` | ✅ 204 `[T1]` | ✅ 204 no-op `[T2]` | ❔ | ❔ |
| **3 Aprovada**, com lançamento | ❔ `[ANÁLISE]` deve dar o mesmo 400 (o bloqueio é por estoque, não por situação) | ✅ →2 204 `[T7]` | ❌ 400 | ✅ 204 | ❔ | ❔ |
| **2 Cancelada** | ❔ | ❔ (2→3 reabre?) | ❔ | ✅ **204 e DEVOLVE** (saldo 1→2) `[T7]` | ❔ | ❔ |
| **1 Faturada** · **4** · **7** · **5** · **6** · **8** · **9** | ❔ | ❔ | ❔ | ❔ | ❔ | ❔ |
| **Pedido apenas RESERVADO (conta A)** | 🔴 **A pergunta de §2.4** | ❔ | ❔ | ❔ | ❔ | ❔ |

**O que a matriz mostra:** o bloqueio real do Tiny é **estoque lançado**, não situação. Nenhum acoplamento entre
situação e estoque é declarado no swagger, e o único acoplamento comprovado é **negativo** (cancelar não devolve).

## 3.5 `PUT /situacao` — enum real, transições e efeitos colaterais

### O enum, verificado em 4 pontos do documento

`AtualizarSituacaoPedidoModelRequest` é **o único schema de request de Pedidos que declara `required`** `[SWAGGER]`:

```json
{"required":["situacao"],
 "properties":{"situacao":{"type":"integer","enum":[8,0,3,4,1,7,5,6,2,9]}},
 "type":"object"}
```

`x-enumDescriptions`, **idênticas em 4 lugares** (query param do GET, `PedidoModel.situacao` do POST,
`ObterPedidoModelResponse.situacao`, e o PUT de situação) `[SWAGGER]`:

| Valor | Descrição literal |
|---|---|
| 8 | Dados Incompletos |
| 0 | Aberta |
| 3 | Aprovada |
| 4 | Preparando Envio |
| 1 | Faturada |
| 7 | Pronto Envio |
| 5 | Enviada |
| 6 | Entregue |
| 2 | Cancelada |
| 9 | Nao Entregue |

O array **não está em ordem numérica** — é a ordem do ciclo de vida do ERP. `[ANÁLISE]` é a máquina de estados do
Tiny de graça, mas só como **sugestão de fluxo**, não como regra imposta. Descrições **sem acento** ("Nao
Entregue") — importa se houver match por string.

Corpo: `{"situacao": 3}` → **204 sem corpo** `[EMPÍRICO 11/07: 34 chamadas]`.

### Transições bloqueadas

**`[SWAGGER]`: zero.** Enum plano, sem `x-transitions`, sem status de erro específico para transição inválida.
Sintaticamente permite `6 Entregue → 0 Aberta` e `2 Cancelada → 3 Aprovada`.

**`[EMPÍRICO 11/07]`: só duas transições foram exercitadas em toda a história do projeto** — `{"situacao":3}` 4×
(todas 204) e `{"situacao":2}` 30× (todas 204). **Nunca houve um único 400 num `PUT /situacao`.** Não existe uma
única evidência de transição recusada, nem a favor nem contra. **88 das 90 células da matriz 10×10 são
desconhecidas** — `[ABERTO]`.

`[EMPÍRICO prod / CÓDIGO tiny.go:1927-1931]` Reaprovar um pedido já aprovado é **inócuo** (idempotente).

### O que cancelar dispara sozinho

| Efeito | Resposta | Fonte |
|---|---|---|
| Devolve estoque lançado? | **NÃO.** `0→2` com estoque lançado: saldo `1 → 1`. `3→2`: idem | `[EMPÍRICO 11/07: T7]` |
| `estornar-estoque` funciona **depois** do cancelamento? | **SIM** — 204, saldo `1 → 2`. Dá uma ordem de recuperação segura | `[EMPÍRICO 11/07: T7]` |
| Estorna contas a receber? | `[ABERTO]` — nada no swagger, nunca chamamos `lancar-contas` | — |
| `→1 Faturada` lança contas sozinho? | `[ABERTO]` | — |

🔴 **Corrida cancelar ∥ lançar — e é um bug do Tiny** `[EMPÍRICO 11/07: T11/C4]`: `PUT /situacao {"situacao":2}`
∥ `POST /lancar-estoque` no mesmo pedido → **ambos 204**, situação final **2 (Cancelada)**, saldo `6 → 5`.
**Pedido cancelado ficou com estoque lançado. Não há guarda nenhuma do lado do Tiny.**

🟠 **A ordem correta do cancelamento é ambígua no NOSSO próprio código** — o tipo de ambiguidade que produz estoque
fantasma:
- `[CÓDIGO tiny.go:2149-2153]` o comentário diz `situacao=2` → `ReverseOrderStock`;
- `[CÓDIGO tiny.go:2046]` `CancelOrder` faz o inverso (estornar → cancelar);
- `[CÓDIGO order_lifecycle.go:644-647]` justifica cancel→estornar citando T7 + a corrida C4.

`[ANÁLISE]` T7 e C4 juntos favorecem **cancelar primeiro, estornar depois**: o estorno funciona em pedido já
cancelado (T7), e cancelar primeiro fecha a janela em que um `lancar-estoque` concorrente entra (C4). Mas a
decisão precisa ser tomada e escrita **num lugar só**.

## 3.6 Como buscar pelo número que o lojista vê

**Dois passos obrigatórios** `[SWAGGER]`. O bloco abaixo é a **forma declarada** do contrato com números
ilustrativos — **não é corpo capturado**; nenhuma busca por `numero` foi executada em 11/07 nem hoje `[ABERTO]`:

```
GET /pedidos?numero=1186   → 200 {"itens":[{"id":<id interno>,"numeroPedido":1186,…}],
                                  "paginacao":{"limit":100,"offset":0,"total":1}}
GET /pedidos/<id interno>  → 200 pedido completo
```

`{idPedido}` do path é o **id interno**. O swagger prova que são coisas distintas: `CriarPedidoModelResponse`
devolve `id` **e** `numeroPedido` separadamente. **Não existe path alternativo** — a varredura confirmou que a tag
Pedidos tem **17 operações distribuídas em 12 paths**, e que esses 12 são exatamente os únicos paths do documento
que contêm "pedido" `[EMPÍRICO 25/08: varredura — recontado nesta revisão; a redação anterior dizia "17 paths",
confundindo operação com path]`.

### As outras âncoras, e por que quase todas estão mortas

| Âncora | Veredito | Fonte |
|---|---|---|
| `ecommerce.numeroPedidoEcommerce` | **MORTO.** Aceito no POST e **descartado**: o GET devolve `""`. O filtro `?numeroPedidoEcommerce=` devolveu `total:0` em **31 tentativas ao longo de 30 s** | `[EMPÍRICO 11/07: T8]` |
| `numeroOrdemCompra` | **Escreve, não lê.** É gravado e persiste no GET (`"numeroOrdemCompra":"LCSBX-0711-1437-13"`), mas **não existe parâmetro de filtro** | `[EMPÍRICO 11/07: T8]` + `[SWAGGER]` |
| **`marcadores`** | **A ÚNICA âncora buscável.** `?marcadores=<tag>` (escalar) → **200 na 1ª tentativa**. `?marcadores[]=<tag>` → **500 `{"mensagem":"Internal Server Error"}` em 30/30 tentativas** — bug determinístico do servidor com sintaxe PHP-style | `[EMPÍRICO 11/07: T8]` |
| `dataInicial` + varredura | Fallback universal: `?dataInicial=2026-07-11&limit=100` → 200, 19 pedidos | `[EMPÍRICO 11/07: #206]` |

⚠️ **Correção formal a um número que circula no nosso código.** `[CÓDIGO tiny.go:2173-2181, providers/types.go:338]`
afirma *"read-after-write de ~300 ms"* para a busca por marcador. **Os 300 ms são o `dur_ms` da chamada HTTP, não a
propagação.** O `POST /marcadores` foi às `18:04:08.306` e a 1ª consulta bem-formada às `18:06:00.562` —
**112 segundos depois**; as 30 tentativas no meio falharam por **sintaxe**, não por propagação `[EMPÍRICO 11/07:
releitura do actions.jsonl]`. **Tudo que está provado: o marcador estava visível em ≤112 s. A propagação real nunca
foi medida.** Qualquer desenho que faça "POST marcador → busca para confirmar posse" está apoiado num número
inexistente. **Chamada que fecharia:** sondar `?marcadores=` a cada 500 ms logo após o POST.

### Filtros disponíveis de `GET /pedidos` — a lista completa `[SWAGGER]`

`numero` (integer) · `nomeCliente` · `codigoCliente` · `cpfCnpj` · `dataInicial` · `dataFinal` ·
`dataAtualizacao` · `situacao` (enum, **um valor só** — não dá para filtrar "aprovada OU faturada") ·
`numeroPedidoEcommerce` · `idVendedor` · `marcadores` (array de string) · `origemPedido` (enum `[0,1]`) ·
`orderBy` (`asc|desc` — **só a direção, não o campo**) · `limit` (default **100**, **sem `maximum` declarado**) ·
`offset`.

⚠️ **A listagem não devolve `itens`, `marcadores`, `numeroOrdemCompra`, `valorFrete` nem `valorDesconto`**
`[SWAGGER — verificado campo a campo contra os corpos reais]`. Qualquer varredura de reconciliação custa
**1 + N chamadas** — o que, a 30 req/min (§6.1), é o gargalo estrutural.

### Abertos da busca

| Pergunta | Marca | Chamada que fecharia |
|---|---|---|
| `numero` é match exato ou parcial? | `[ABERTO]` | `GET /pedidos?numero=118` num conjunto com 118 e 1186 |
| O número é único por conta? | `[ABERTO]` — `[EMPÍRICO prod #1087]` o número **27187 foi reciclado** para outro cliente 1 h após uma exclusão. **A numeração não é estável** | criar, excluir, criar e comparar |
| Sem resultado: 200 com `itens:[]` ou 404? | **RESPONDIDO: 200 com `itens:[]`** | `[EMPÍRICO 25/08: GET /pedidos?limit=1 → {"itens":[],"paginacao":{"limit":1,"offset":0,"total":0}}]` |
| `marcadores=a&marcadores=b` é AND ou OR? exato ou substring? | `[ABERTO]` | duas tags no mesmo pedido + consulta com as duas |
| `numero` combina com `situacao` por AND? | `[ABERTO]` | — |

## 3.7 Idempotência da criação — o 409

**Não existe `Idempotency-Key`.** `idempot` = 0 ocorrências. Mais forte: **não existe UM ÚNICO parâmetro
`in: header`** no arquivo inteiro — além do `Authorization`, a spec não define header nenhum
`[EMPÍRICO 25/08: contagem]`.

**Existe dedup nativa, por hash de payload, e ela NÃO é atômica** `[EMPÍRICO 11/07: 13 × 409]`:

```
POST /pedidos  → 409  {"mensagem":"Esse registro já existe"}
```

Como foi deduzida, cruzando os 42 POSTs:

| Evidência | Conclusão |
|---|---|
| `#17` (201) e `#24` (409) têm corpo **byte-idêntico** | a dedup é sobre o **conteúdo do payload** |
| `#55`/`#56` (201) diferem de `#54` (409) **só** por ter o objeto `ecommerce` | `ecommerce` **entra** no hash |
| `#89` (201, `numeroOrdemCompra=LCSBX-0711-1437-1`) e `#222` (409, mesmo valor) | `numeroOrdemCompra` **entra** no hash |
| `#222` às 18:15 colidiu com `#89` às 17:54 | janela **≥ 21 minutos** — não é cache curto |
| `#266` (201) idêntico a `#222` exceto `numeroOrdemCompra` novo | trocar 1 campo derruba a dedup |

`[ANÁLISE, coerente com 42/42 observações]` a chave é um hash de (contato + data + itens + observações +
`numeroOrdemCompra` + `ecommerce`), com janela longa. Dois compradores diferentes **não** colidem; um mesmo
carrinho reenviado **colide**.

🔴 **Mas sob concorrência ela falha** `[EMPÍRICO 11/07: T11/C5]` — 5 POSTs idênticos simultâneos:

```
g1 → 201  (358130583)     g2 → 409
g3 → 201  (358130585)     g4 → 429  → no retry, 409
                          g5 → 429  → no retry, 409
```

**Dois pedidos duplicados foram criados. A dedup segurou 1; o RATE LIMIT segurou 2.** Sem o 429, o resultado
plausível seria mais duplicatas. *(O report original resume isto como "dedup segurou 3" — é otimista demais.)*

🔴 **O 409 NÃO devolve o id do pedido existente.** O corpo é só `{"mensagem":"Esse registro já existe"}`.
**Um 409 nos deixa cegos: sabemos que existe, não sabemos qual é.** É a diferença entre um retry se recuperar
sozinho e virar pedido órfão — foi o `847978450` / carrinho #1087 em produção `[EMPÍRICO prod]`.

⚠️ **`409` não é declarado no swagger em nenhuma das 202 operações** `[EMPÍRICO 25/08: contagem]`.

**Abertos:** a janela é a data do pedido, 24 h ou permanente? Pedido **cancelado** continua bloqueando um POST
idêntico? `[ABERTO]` — **chamada que fecharia:** POST idêntico → cancelar → POST idêntico de novo, e um terceiro
no dia seguinte.

---

# 4. Estoque

## 4.1 `GET /estoque/{idProduto}`

**Nenhum query param** — não filtra por depósito nem por data `[SWAGGER]`. Corpo transcrito de hoje
`[EMPÍRICO 25/08]`:

```json
{"id":357281337,"nome":"Console PlayStation® 5 Slim Edição Digital 825 GB Branco - Sony",
 "codigo":"","unidade":"UN","saldo":1,"reservado":0,"disponivel":0,"localizacao":"",
 "depositos":[{"id":334779581,"nome":"Geral","desconsiderar":false,
               "saldo":1,"reservado":0,"disponivel":0,"empresa":"59573950000158"}]}
```

O schema do swagger bate **exatamente** com esse corpo `[SWAGGER + EMPÍRICO 25/08]`.
⚠️ `[CÓDIGO tiny.go:530-534]` o comentário diz *"o schema não está na documentação pública"* — **está, e documenta
exatamente o que o parser adivinhou.** Comentário obsoleto, apagar.

### Semântica dos três números

| Campo | Semântica | Fonte |
|---|---|---|
| `saldo` | **físico**. Igual a `GET /produtos/{id}.estoque.quantidade` (10 vs 10 no mesmo instante, `#41` vs `#42`) | `[EMPÍRICO 11/07: T4]` |
| `disponivel` | vendável. **Pode vir negativo, e negativo é esgotado** — descartar o negativo foi a raiz do estoque negativo de 20/08 (corpos reais: `saldo:1, reservado:2, disponivel:-1`) | `[EMPÍRICO prod 20/08]` |
| `reservado` | reservas de **documento da própria Tiny**, nunca as nossas saídas manuais | `[EMPÍRICO 11/07 + prod]` |

⚠️ **O `disponivel` da raiz NÃO é a soma dos depósitos**: raiz `disponivel:-1` com depósitos `-2` e `-1`
`[EMPÍRICO prod 20/08]`.

## 4.2 `POST /estoque/{idProduto}`

**required: `["tipo","quantidade","precoUnitario"]`** `[SWAGGER]` — note que **`precoUnitario` é obrigatório mesmo
numa saída**. Opcionais: `deposito{id}`, `data`, `observacoes`.
Resposta **200** (não 201): `{"idLancamento": <int>}` `[SWAGGER + EMPÍRICO 11/07, 10/10 chamadas]`.

### Tipos: delta vs absoluto

`[EMPÍRICO 11/07: T4]` — sequência real sobre o produto `339086740`:

| Requisição | Resposta | saldo |
|---|---|---|
| `{"tipo":"B","quantidade":10,"precoUnitario":10,…}` | `200 {"idLancamento":358126257}` | **465 → 10** |
| `{"tipo":"E","quantidade":1,…}` | `200 {"idLancamento":358126607}` | 10 → 11 |
| `{"tipo":"S","quantidade":1,…}` | `200 {"idLancamento":358126743}` | 11 → 10 |

**`S` subtrai · `E` soma · `B` FIXA o saldo (absoluto).** O swagger **não descreve `quantidade` em lugar nenhum** —
a semântica é 100% empírica. `x-enumDescriptions`: `["B - Balanco","E - Entrada","S - Saida"]`.

### 🔴 Não existe dedup

`[EMPÍRICO 11/07: T10]` — corpo **byte-idêntico** enviado 2×:

```
E +3 (1ª)  → 200 {"idLancamento":358129445}   saldo 10 → 13
E +3 (2ª)  → 200 {"idLancamento":358129482}   saldo 13 → 16
B  10      → 200 {"idLancamento":358129496}   saldo 16 → 10
```

**Retry cego de `POST /estoque` duplica — sempre.** É este achado que justifica o estado `unconfirmed` inteiro:
sem dedup nativa e sem `Idempotency-Key`, um timeout é irrecuperável **sem prova externa**. E a única prova externa
candidata é o `logs-movimentacao` (§4.3).

### `maxLength` de `observacoes`

**O swagger é inútil aqui:** o arquivo de 1,1 MB tem **ZERO ocorrências de `maxLength`, `minLength` e `pattern`**
`[EMPÍRICO 25/08: contagem]`.

`[EMPÍRICO prod]` As strings que hoje escrevemos em `observacoes`, com o comprimento observado:
```
"@%s - Cart %s"                                              erp/movement_ledger.go:161
"Cart %s (retry %d)"                                         erp/movement_ledger.go:159
"Estorno LiveCart [%s] - Cart %s (retry %d)"                 erp/movement_ledger.go:361
"Estorno reserva pós-pagamento - Cart %s"                    erp/finalisation.go:192
"Estorno expiração carrinho LiveCart - Cart %s - Reserva %s" erp/finalisation.go:513  ← ~85 chars, dois UUIDs
"Re-reserva pós-falha de finalização - Cart %s"              integration/service.go:4380
```

**Limite real ≥ ~85 chars; o teto é desconhecido** — `[ABERTO]`. **Chamada que fecharia:** POST com observação de
100 / 255 / 256 / 1000 chars **e ler de volta pelo `logs-movimentacao`**, não só conferir o 200. O risco silencioso
é **truncar no meio de um UUID** e quebrar o casamento sem erro visível.

### Outros abertos do POST

| Pergunta | Marca | Chamada que fecharia |
|---|---|---|
| `deposito` omitido → qual depósito? | `[ABERTO]` — `[CÓDIGO tiny.go:2226-2231]` omitimos **sempre**; a Canto da Art tem "galpão (estoque)" e "loja". **Toda a nossa base empírica é de conta MONO-DEPÓSITO** (ADABYTE tem 1: `{334779581,"Geral"}`) — não transfere | um POST **com** `deposito.id` e um **sem**, lendo `GET /estoque/{id}.depositos[]` depois de cada |
| Formato de `data` no POST | `[ABERTO]` — é o **único campo de data do arquivo inteiro sem `example` e sem `format`**. Dá para lançar retroativo? | POST com `data` de ontem e conferir no log |
| `quantidade` negativa | `[ABERTO]` — é float. Um `E` com quantidade negativa equivale a um `S`? | POST `{"tipo":"E","quantidade":-1,…}` |
| Kits e variações | `[ABERTO]` — `lancar-estoque` num kit baixa o kit ou os componentes? `estoque.controlar=false` e `sobEncomenda=true` mudam a semântica de saldo. Ambos os produtos da bateria são `tipoVariacao:"N"` | um pedido com kit e um com variação |

## 4.3 `logs-movimentacao` — o que sabemos e o que não

**Status na nossa conta: 403.** `GET /estoque/357281337/logs-movimentacao` e `…?limit=5` → **403 nas duas
tentativas de hoje** `[EMPÍRICO 25/08]`, com o JWT carregando `estoque-leitura` entre as 48 roles. **É bloqueio de
conta/plano.** Esta conta **não pode** responder o shape.

**Parâmetros** `[SWAGGER]`:

| Nome | in | Tipo | Default | Descrição literal |
|---|---|---|---|---|
| `idProduto` | path | integer | — | "Identificador do produto" |
| `dataInicio` | query | string, `example "2026-01-01"` | — | "Data inicial do período (Y-m-d). Quando dataInicio e dataFim são informadas, a busca é por período; caso contrário, retorna as últimas movimentações." |
| `dataFim` | query | string | — | "Data final do período (Y-m-d)." |
| `tipo` | query | enum `["E","S","B"]` | — | "Tipo de movimentação…" |
| `idDeposito` | query | integer | — | "(opcional)" |
| `limit` | query | integer | **100** | — |
| `offset` | query | integer | **0** | — |

**Campos declarados do item (`LogMovimentacaoEstoqueResponseModel`)** `[SWAGGER]`:

```
id · idProduto · idDeposito · tipo (E|S|B) · quantidade (float) · preco (float)
data ("2026-01-15 14:30:00" — COM hora) · observacao (SINGULAR)
usuario ("Nome do usuário autor… vazio quando não rastreado")
origem ("Código do canal de origem… ex.: ajuste_manual, pdv, api")
origemDescricao · idOrigem ("Identificador do documento de origem (quando houver)")
objOrigem ("Tipo do documento de origem (quando houver)")
```

### 🔴 Duas ressalvas que impedem planejar em cima disso

1. **A resposta 200 não declara os itens.** Verificado hoje `[EMPÍRICO 25/08]`: o 200 é
   `{"$ref":"…/PaginatedResultModel"}`, e `PaginatedResultModel` resolve para **exatamente `{limit, offset,
   total}`** — sem array. Todas as outras listagens do arquivo usam `{itens:[…], paginacao:{…}}`. E de 202
   operações, **exatamente uma** devolve `PaginatedResultModel` puro: esta. `[ANÁLISE]` é quase certamente bug de
   anotação do fornecedor, mas o nome do wrapper e o aninhamento **têm de ser vistos antes de escrever o parser**.
2. **O schema do item é órfão** (0 referências). ⚠️ **Isso NÃO é sinal de "feature recém-lançada"**: há **13
   schemas órfãos** no arquivo, entre eles `BaseTagModel`, `ContatoPedidoModel` e `MarcaProdutoModelResponse` —
   features antigas e vivas `[EMPÍRICO 25/08: script]`. **Orfandade aqui é ruído de anotação.** A pergunta empírica
   continua válida pelo outro motivo (a resposta 200 não declara os itens), não por este.

### As sete perguntas que a chamada real responde

`[ABERTO]` — **teste T-L1**, numa conta com permissão:

```
POST /estoque/{idProduto}  {"tipo":"E","quantidade":1,"precoUnitario":10,
                            "observacoes":"LAB-T-L1-<uuid-de-36-chars>"}     → guardar idLancamento
GET  /estoque/{idProduto}/logs-movimentacao?dataInicio=YYYY-MM-DD&dataFim=YYYY-MM-DD&tipo=E&limit=100
                                                                              → capturar o 200 INTEIRO, cru
```

1. Responde 200 ou 403/404 nesta conta?
2. O array chama-se `itens`? A paginação vem em `paginacao` ou na raiz?
3. `item.id` == o `idLancamento` do POST? *(o swagger não afirma isso em lugar nenhum)*
4. **`item.observacao` ecoa fielmente o `observacoes` que enviamos**, ou trunca/prefixa/vem vazio para lançamentos
   de API? ⚠️ **Atenção à assimetria de nome: o POST envia `observacoes` (plural) e o log devolve `observacao`
   (singular).** *(Terceira assimetria do mesmo domínio: o POST envia `precoUnitario`, o log devolve `preco`.)*
5. Qual a **latência de indexação** — aparece logo após o 200, ou há atraso? Define se serve como resolver
   pós-timeout ou só como auditoria.
6. `origem == "api"` para o nosso lançamento? `objOrigem`/`idOrigem` vêm **nulos** num `POST /estoque` avulso e
   **preenchidos** num `lancar-estoque` de pedido? Se sim, isso sozinho discrimina "reserva LiveCart" de "baixa por
   pedido" — e dá o rastreio movimento → pedido, que hoje não existe.
7. Conta contra qual balde de rate limit (§6.1)?

**Se `observacao` ecoar fielmente, a classe `unconfirmed` inteira vira uma consulta determinística**, o gate que
trava a finalização de carrinho pago some, e o painel de resolução manual deixa de ser necessário.

⚠️ **Limitações estruturais já conhecidas, mesmo que tudo funcione** `[SWAGGER]`:
- **Não há filtro por `idOrigem`, por `observacao` nem por `idLancamento`.** Localizar "o meu lançamento" exige
  varrer a janela de datas do produto e casar no cliente.
- **Não há saldo resultante.** É uma lista de **deltas com autoria**, não um extrato com saldo corrente.
- O filtro tem granularidade de **dia** (`Y-m-d`) e o retorno tem **hora** (`Y-m-d H:i:s`).
- Sem `orderBy` — a ordem não é declarada.
- **Não enumera os valores de `objOrigem`.** Não dá para saber se um pedido aparece como `"pedido"`, `"venda"` ou
  outra coisa, nem se `idOrigem` é o `idPedido` `[ABERTO]`. **Pista dentro do próprio arquivo, encontrada nesta
  revisão** `[SWAGGER]`: `NotaFiscalOrigemModelResponse.tipo` enumera um vocabulário de tipo-de-documento-de-origem
  — `pedido_compra` · `venda` · `notafiscal` · `ordemservico` · `cobranca` · `devolucao`. **Não há nada no arquivo
  ligando esse enum a `objOrigem`** — é hipótese a testar junto com T-L1, não resposta.

## 4.4 Lançar / estornar estoque do pedido — o comportamento comprovado

`POST /pedidos/{idPedido}/lancar-estoque` e `/estornar-estoque`: **sem requestBody**, sucesso **204 No Content**,
único parâmetro `idPedido`, **zero `description`** no swagger `[SWAGGER]`. Tudo abaixo é empírico.

`[EMPÍRICO 11/07: T2, actions.jsonl#89..113]`

| Ação | HTTP | saldo | Corpo |
|---|---|---|---|
| `lancar` 1ª vez | **204** | 7 → 6 | — |
| **`lancar` 2ª vez** | **400** | 6 → 6 | `{"mensagem":"Estoque já lançado."}` — **com ponto final** |
| `estornar` 1ª vez | **204** | 6 → 7 | — |
| **`estornar` 2ª vez** | **204** | 7 → 7 | **não infla** |
| `lancar` de novo pós-estorno | **204** | 7 → 6 | re-lançamento permitido |
| **`estornar` SEM lançamento prévio** | **204** | 6 → 6 | **no-op silencioso** |
| **`lancar` ×2 CONCORRENTE** (`#111`,`#112`, mesmo ts `17:56:49.949`) | g1 **204** / g2 **400** | 6 → 5 | **Δ = 1, não 2** |

**As três regras que saem daí:**
1. **`lancar-estoque` é atômico e guardado.** O `400 "Estoque já lançado."` é **sinal de idempotência satisfeita,
   não de falha.** `[CÓDIGO tiny.go:1981]` tratamos como sucesso via `strings.Contains(msg,"já lançado")` — está
   certo, mas é matcher por substring, frágil a mudança de texto do fornecedor.
2. **`estornar-estoque` é idempotente, tolerante a órfão e funciona em pedido já cancelado.** **Retry cego de
   estorno é seguro.** *(Retry cego de lançamento também é — mas só porque o 400 protege.)*
3. **Cancelar NÃO devolve estoque lançado** (T7). O par cancelamento+estorno tem de ser nosso.

### Quando o Tiny lança estoque sozinho — configuração de conta, e não sabemos qual toggle

| Conta | Comportamento | Fonte |
|---|---|---|
| **ADABYTE** | nem criar nem aprovar movem estoque; **só `lancar-estoque` move**, e ele **não** altera a `situacao` (o pedido continuou `0` após o lançamento) | `[EMPÍRICO 11/07: T1]` |
| **Produção** | no pedido `847982356` o nosso log diz **"stock already launched by Tiny automatically"** | `[EMPÍRICO prod 25/08]` |

**Comportamentos opostos, e não sabemos qual toggle controla** — `[ABERTO]`.

### Saldo negativo — também é configuração de conta

| Conta | Comportamento | Fonte |
|---|---|---|
| **ADABYTE** | saída `S` zerou o saldo → `POST /pedidos` com saldo 0 → **201** → `lancar-estoque` → **204, saldo −1, sem erro** | `[EMPÍRICO 11/07: T3 round 2, #264..275]` |
| **Produção** | `lancar-estoque` → **400 `"saldo em estoque de um ou mais produtos é insuficiente"`** no pedido #1186 | `[EMPÍRICO prod 25/08]` |

Os reports de 11/07 registram só os rótulos "config-atual"/"config-atual-v2", **não os toggles** — `[ABERTO]`.

⚠️ **Nota de método sobre o T3 round 1:** o report registra `409 | pedido id=0` e sugere bloqueio por estoque. O
corpo real (`actions.jsonl#222`) é `{"mensagem":"Esse registro já existe"}` com um `numeroOrdemCompra` já usado às
17:54:43. **Foi colisão de dedup, não bloqueio por estoque. O round 1 não mediu nada.**

---

# 5. Contas / financeiro — território virgem

`[CÓDIGO: grep]` `grep -rn "lancar-contas\|estornar-contas\|contas-receber\|contas-pagar" --include=*.go` no
`livecart-be` = **0 linhas**. **Nunca chamamos nada disso.** Tudo nesta seção é `[SWAGGER]` ou `[ABERTO]`.

## 5.1 Lançar / estornar contas do pedido

`POST /pedidos/{idPedido}/lancar-contas` e `/estornar-contas` `[SWAGGER]`: **sem requestBody**, **204 No Content**,
único parâmetro `idPedido`, **zero `description`** — só o summary ("Lançar contas do pedido" / "Estornar contas do
pedido"). São **idênticos em forma** ao par de estoque.

| Pergunta | Marca | Chamada que fecharia |
|---|---|---|
| O que `lancar-contas` gera — uma conta por parcela ou uma só? | `[ABERTO]` — o swagger não diz nada; o 204 **não devolve os ids criados** | `lancar-contas` → `GET /contas-receber?idVenda={idPedido}` |
| Gera a partir de `pagamento.parcelas` ou de `pagamentosIntegrados`? | `[ABERTO]` | pedido com parcelas ∥ pedido com pagamentosIntegrados |
| É idempotente? Chamar 2× duplica os títulos? | `[ABERTO]` — **mesmo risco do `POST /estoque`** | `lancar-contas` 2× → contar em `?idVenda=` |
| `estornar-contas` com conta **já baixada** funciona, ou exige desfazer a baixa antes? | `[ABERTO]` — e **desfazer a baixa é operação que não existe** (§5.3) | baixar → `estornar-contas` |
| Cancelar pedido estorna contas automaticamente? | `[ABERTO]` — sabemos que **não** estorna estoque (T7); para contas é incógnita | `lancar-contas` → `situacao=2` → `GET /contas-receber?idVenda=` |
| Existe bloqueio simétrico ao "estoque lançado" no `PUT /itens`? | `[ABERTO]` `[ANÁLISE]` plausível pela simetria das quatro operações, não verificado | `lancar-contas` → `PUT /itens` |

✅ **Uma diferença favorável em relação ao `POST /estoque`: aqui EXISTE consulta de volta.**
`GET /contas-receber?idVenda={id}` — descrição literal *"Pesquisa por identificador da venda de contas a receber"*
`[SWAGGER]`. Ou seja, `lancar-contas` **pode ser desenhado com verificação pós-fato**, coisa que a reserva por
`POST /estoque` não permite hoje. ⚠️ `[ABERTO]` o swagger **não afirma que "venda" == `idPedido`** — o mesmo
endpoint tem um `idNota` separado. Uma chamada resolve.

## 5.2 Onde entram frete e forma de pagamento

### Frete — dois lugares independentes, ambos só na criação `[SWAGGER]`

1. **`valorFrete`** — float escalar de topo, **sem `description`**.
2. **`transportador{}`** — `id`, `fretePorConta` (enum `R/D/T/3/4/S`), `formaEnvio{id}`, `formaFrete{id}`,
   `codigoRastreamento`, `urlRastreamento`.

**Participa do `valorTotalPedido`?** `[ABERTO]` — `valorTotalPedido` e `valorTotalProdutos` **não têm
`description`**. A fórmula existe no mesmo arquivo, mas **para orçamentos**:
`ObterOrcamentoModelResponse.valorTotal` = *"Valor total da proposta (itens + extras - desconto + frete +
impostos)"*. **Indício forte sobre outro recurso; não é afirmação sobre pedido.**

**Retirada em loja:** `FormaFreteModel.tipoEntrega = 6 (Retirada)` — **só aparece em `GET /formas-envio/{id}`
(detalhe), nunca na listagem** `[SWAGGER]`. Achar a forma de retirada custa 1 listagem + N detalhes. `[ABERTO]` se
existe por padrão nas contas ou se o lojista precisa cadastrar.

### Forma de pagamento e parcelas `[SWAGGER]`

```json
"pagamento": {
  "formaRecebimento": {"id": 0},        // nível PEDIDO
  "meioPagamento":    {"id": 0},        // nível PEDIDO
  "categoria":        {"id": 0},
  "parcelas": [ {"dias":0, "data":"2024-01-01", "valor":0.0, "observacoes":"…",
                 "formaRecebimento":{"id":0}, "meioPagamento":{"id":0}} ]   // nível PARCELA
}
```

**`pagamento.categoria.id` é a ÚNICA regra funcional escrita em todo o bloco de pagamento** `[SWAGGER]`, citada
literal:
> *"Identificador da categoria de receita ou despesa, obtido em `/categorias-receita-despesa`. **Envie 0 para
> gravar o pedido sem categoria.** Se o objeto não for enviado, a criação assume a categoria padrão de venda da
> conta e a atualização mantém a categoria atual do pedido."*

`0` é sentinela válido — é aproveitável.

🔴 **`formaRecebimento` do PEDIDO é imutável pelo `PUT /pedidos`** `[SWAGGER]`: o POST usa
`PedidoPagamentoRequestModel` (com `formaRecebimento` + `meioPagamento` no nível do pedido); o PUT usa
`PagamentoParcelasRequestModel` (só `parcelas` + `categoria`). Combinado com a validação empírica de que **a
`formaRecebimento` da parcela tem de bater com a do pedido** `[EMPÍRICO 11/07: E2E]`, isso significa: **se
quisermos categorização correta em contas a receber, ela tem de ser decidida no POST.** Hoje o pedido nasce sem
pagamento e `[CÓDIGO tiny.go:2127-2136]` **omite `formaRecebimento` da parcela de propósito**, aceitando a
categorização genérica.

*(Ver a hipótese testável do default "Múltiplas" em §3.1 — pode ser a explicação inteira desse 400.)*

`[ABERTO]` **`pagamentosIntegrados` × `parcelas`**: somam, substituem, ou são dimensões independentes (financeiro
vs fiscal)? O swagger não explica a relação.

### `/formas-pagamento` e `/formas-recebimento` — o cadastro real, lido hoje

Os dois pares são idênticos em contrato `[SWAGGER]`: filtros `nome`, `situacao` (enum `[1,2]` =
Habilitada/Desabilitada), `limit`, `offset`; modelo `{id, nome, situacao}`; **somente leitura**.

**Corpo real de hoje** `[EMPÍRICO 25/08: GET /formas-pagamento]`:

```json
{"itens":[{"id":334779586,"nome":"Dinheiro","situacao":"1"},
          {"id":334779587,"nome":"Cartão de crédito","situacao":"1"},
          {"id":334779588,"nome":"Cartão de débito","situacao":"2"},
          {"id":334779589,"nome":"Boleto","situacao":"2"},
          {"id":334779590,"nome":"Cheque","situacao":"1"},
          {"id":334779591,"nome":"Depósito","situacao":"2"},
          {"id":334779592,"nome":"Crediário","situacao":"2"},
          {"id":334779593,"nome":"Vale-troca","situacao":"2"},
          {"id":334779594,"nome":"Pix","situacao":"1"},
          {"id":351409700,"nome":"Cashback","situacao":"2"},
          {"id":358389334,"nome":"Vale-presente","situacao":"2"}],
 "paginacao":{"limit":100,"offset":0,"total":11}}
```

Três coisas que esse corpo prova `[EMPÍRICO 25/08]`:
1. **`situacao` vem como STRING** (`"1"`, `"2"`), apesar de o swagger declarar `type: string` com `enum: [1,2]`
   **inteiros**. Parsear como string.
2. **O filtro `situacao` é útil**: metade do cadastro desta conta está **desabilitada** (`"2"`). É exatamente o
   discriminador que evitaria o *"Forma de envio não habilitada"* que matou uma venda paga em 16/08.
3. `[CÓDIGO tiny.go:1733-1742]` diz *"o endpoint `/formas-recebimento` não aceita um filtro `nome`, só
   limit/offset"* e por isso **pagina até 5 páginas de 100 por pedido criado**. `[SWAGGER]` **os três endpoints
   aceitam `nome` E `situacao`.** Comentário obsoleto que custa **1 a 5 chamadas por pedido** dentro do orçamento
   de 90 s. **Chamada que fecharia:** `GET /formas-recebimento?nome=Pix&situacao=1`.

⚠️ `[CÓDIGO tiny.go:2863-2869, jul/2026]` **a nomenclatura da Tiny é invertida em relação ao painel**:
`/formas-pagamento` valida o que o painel exibe em "Formas de RECEBIMENTO" e vice-versa.

## 5.3 Efeito sobre parcelas e o estorno no nível da conta

### Parcelas

- `PUT /pedidos/{id}` **aceita** `pagamento.parcelas[]` e é **merge** `[EMPÍRICO 11/07: T5]`.
- `PUT /itens` **recalcula os valores das parcelas existentes** `[SWAGGER + EMPÍRICO 11/07: T6-d, com a ressalva
  de §3.2]`.
- `[ABERTO]` distribuição com N>1 parcelas; criação de parcela quando o pedido tem 0.

### 🔴 Estornar é operação de PEDIDO, não de conta

As 10 operações de Contas a receber **não incluem** desfazer-baixa, estornar nem `DELETE` `[SWAGGER]`:

```
GET  /contas-receber                      POST /contas-receber
GET  /contas-receber/{id}                 PUT  /contas-receber/{id}
POST /contas-receber/{id}/baixar          GET  /contas-receber/{id}/recebimentos
GET|POST|PUT|DELETE /contas-receber/{id}/marcadores
```

`PUT /contas-receber/{id}` aceita **só 5 campos** — `taxa`, `dataVencimento` (**required**), `categoria`,
`dataCompetencia`, `atualizarContaRecorrente` — e **nenhum mexe em situação**. `situacao: "cancelada"` existe no
enum de **leitura**, mas **nenhum endpoint escreve situação de conta**.

**Consequência de arquitetura `[ANÁLISE]`: qualquer reembolso parcial fica amarrado ao grão do pedido inteiro.**
Não há como estornar um título e deixar os outros.

⚠️ **Por que esta inferência a partir de ausência é legítima e a do "frete imutável" (§3.3) não é:** aqui a ausência
é de **operação** — não se chama um endpoint que não existe, e a lista de 10 operações é fechada por construção.
Lá a ausência é de **campo** num documento que nunca declara `additionalProperties`, onde o servidor pode aceitar
o que o schema não lista. `[ABERTO]` residual: se existe rota não documentada, só o fornecedor responde.

### Enum de situação de conta a receber `[SWAGGER]`

`aberto` · `cancelada` · `pago` · `parcial` · `prevista` · `atrasadas` · `emissao`
⚠️ Note as inconsistências de gênero/número do fornecedor (`aberto` masc. sing., `cancelada` fem. sing.,
`atrasadas` fem. **plural**). São strings literais — copiar exatamente.

### Baixar conta — duas armadilhas `[SWAGGER]`

`POST /contas-receber/{id}/baixar`, `requestBody.required: **false**`, nenhum campo required:

| Campo | Tipo | Armadilha |
|---|---|---|
| `data` | string, `example "01/01/2024"` | ⚠️ **`d/m/Y` — formato DIFERENTE de todo o resto do arquivo** (que usa `Y-m-d`) |
| `taxa` / `juros` / `desconto` / `acrescimo` | **`type: string`** | ⚠️ enquanto `valorPago` é `number/float`. `[ABERTO]` qual formato — `"10.50"` ou `"10,50"`? |
| `contaDestino{id}` · `categoria{id}` · `historico` | — | — |

### Outros pontos

- **`GET /contas-receber/{id}` não tem `idVenda`/`idPedido` na resposta** `[SWAGGER]` — **o link é só de ida**
  (filtro na listagem). Dada uma conta, o swagger não oferece o pedido que a originou.
- **`POST /contas-receber` (avulsa)** declara `required: ["dataVencimento","valor","contato"]` e **não tem
  `idVenda` no corpo** — uma conta criada por aqui **nasce órfã do pedido**. O único carimbo possível é
  `historico`, `numeroDocumento` ou um marcador. ⚠️ Aqui `formaRecebimento` é **integer escalar**, divergindo do
  resto do arquivo onde é `{id}`.
- **Marcadores em conta existem** e são filtráveis (`GET /contas-receber?marcadores=` +
  `POST /contas-receber/{id}/marcadores` com `[{descricao}]`) — é o único carimbo próprio possível numa conta.
  `[ABERTO]` AND ou OR? exato ou parcial? limite?
- `GET /contas-receber/{id}/recebimentos` devolve **array cru, sem paginação**; `idConta` é **string** enquanto
  `id` é integer; `tipo` é integer **sem enum e sem descrição** `[SWAGGER]` `[ABERTO]`.
- **Hoje a conta ADABYTE responde `GET /contas-receber?limit=1` → `{"itens":[],"paginacao":{"limit":1,"offset":0,
  "total":0}}`** `[EMPÍRICO 25/08]` — o endpoint está acessível e usa o envelope padrão. **É a conta certa para
  testar o par lançar/estornar contas**, já que a leitura funciona.

---

# 6. Operacional

## 6.1 Rate limit — a medição de hoje

**Esta é a seção que mais muda o dimensionamento do fluxo alvo.** Tudo abaixo foi medido em 25/08/2026 contra a
conta real com uma **rajada de 45 requisições concorrentes + 12 sequenciais**. Evidência crua em
`scratchpad/ratelimit-burst.json` (45 linhas com status + headers completos).

### São DOIS baldes independentes `[EMPÍRICO 25/08]`

| Balde | Limite | Janela | Como o 429 se identifica |
|---|---|---|---|
| **rajada** | **4 requisições** | **1 s** | `X-Ratelimit-Limit: 4`, `X-Ratelimit-Remaining: 0`, `X-Ratelimit-Reset: 1` |
| **sustentado** | **30 requisições** | **60 s** | `X-Ratelimit-Limit: 30`, `X-Ratelimit-Remaining: 0`, `X-Ratelimit-Reset: 58→0` |

Distribuição das 45 respostas da rajada: **8 × 200** (todas com `Limit: 30`), **22 × 429 com `Limit: 4`** (balde de
rajada estourado), **15 × 429 com `Limit: 30`** (balde sustentado estourado, `Reset: 16` decrescente).

**Sustentado = 0,5 req/s de média.**

### Os fatos operacionais que saem daí `[EMPÍRICO 25/08]`

1. **NÃO EXISTE header `Retry-After`.** Foi procurado explicitamente em todas as 45 respostas: `null` em 45/45.
   **O único sinal de recuperação é `X-Ratelimit-Reset`** (segundos restantes na janela).
2. **Os headers estão presentes em TODA resposta — 200, 403 e 429.** Isso é o que permite throttle proativo em vez
   de reativo.
3. **A grafia é `X-Ratelimit-*`** (R e L minúsculos após o hífen). HTTP header names são case-insensitive e o
   `net/http` do Go canonicaliza, então isso não é um problema em Go — mas **é** um problema para qualquer
   comparação de string crua.
4. **Recuperação é automática ao rolar a janela.** Não há bloqueio prolongado, não há penalidade acumulada.
5. **Existe também um `X-Request-Id` em toda resposta** (ex.: `8d5f0d77-ce9f-44f6-a0cd-0c8d113081dc`)
   `[EMPÍRICO 25/08]` — não documentado, e **é o identificador que um ticket de suporte com o fornecedor exige**.
   Nada no nosso código o registra hoje. Vale logar.
6. **O swagger não menciona rate limit em lugar nenhum**: `429`, `RateLimit`, `Retry-After` → **0 ocorrências cada**
   em 1,1 MB `[EMPÍRICO 25/08: contagem]`. As 202 operações declaram só 200/204/400/401/403/404/500/503.
7. **O corpo do 429 é vazio.** `[EMPÍRICO 11/07: 4/4, `resp: null` no log]` — quem faz `json.Unmarshal` no corpo do
   429 não recebe mensagem nenhuma.

### 🔴 A consequência de escala — o fluxo alvo não cabe sem coalescing

**Uma live de 3 h tem orçamento total de ~5.400 chamadas para TUDO** (30 req/min × 180 min).

Com **~600 compradoras × ~3 itens ≈ 1.800 itens adicionados**, um `PUT /pedidos/{id}/itens` por item adicionado
consome **1.800 chamadas — mais de um terço do orçamento sozinho** — e isso antes de contar as leituras de estoque,
a resolução de contato, o `POST /pedidos`, o marcador, a aprovação e o lançamento de cada carrinho. Somando o
caminho de finalização (§ abaixo), **estoura.**

**O fluxo alvo não cabe sem coalescing — debounce por carrinho, com uma janela que agrupe as mutações de uma
compradora numa única chamada de grade.** Isso não é otimização; é requisito de viabilidade.

Reforçando o gargalo: **não existe busca em lote em lugar nenhum da API.** Revisão de todos os filtros de todas as
listagens: não há `codigos[]`, `ids[]` ou equivalente `[SWAGGER]`. **N SKUs = N chamadas.** Combinado com 0,5 req/s
médio, isso obriga **cache local do catálogo (mapa SKU→id) e fila serializada por conta Tiny**, em vez de chamadas
sob demanda.

### O que a medição de hoje corrige no que já estava escrito

| Afirmação anterior | Correção `[EMPÍRICO 25/08]` |
|---|---|
| *"~1 req/s"* `[CÓDIGO tiny.go:31-33, estoqueThrottleBackoff = 1200ms]` | **Errado por 2×.** O sustentado é 0,5 req/s. Uma cadência de 1 req/s toma 429 por design. |
| *"60/120/240 req/min por conta conforme plano"* `[docs/integrations-rate-limiting.md]` | **Não bate com a medição.** Esta conta é 30/min. Ou o doc está desatualizado, ou o número varia por plano — e então o limiter **tem de ser aprendido dos headers**, não configurado. |
| *"40 req/min já estourou"* `[EMPÍRICO 11/07]` | **Consistente.** 40 > 30. E os dois 429 simultâneos de 18:17:06 são o **balde de rajada** (4/s), não o sustentado — a hipótese de "limite de concorrência" levantada no recon está **explicada**. |
| 🔴 *"pode não existir throttle nenhum hoje"* `[CÓDIGO lib/ratelimit/adaptive.go:52-56 e :58-63]` — sem `X-RateLimit-Remaining`, `hasAPIData=false` e `Allow()` **libera tudo**; e quando a janela rola, `:58-63` **zera o estado e libera tudo de novo** | **O header EXISTE e vem em toda resposta.** Então o `AdaptiveLimiter` **pode** funcionar — mas `[CÓDIGO providers/base.go:148-154]` só chama `UpdateFromHeaders` **se** a resposta trouxer o header, e **apenas 4 dos 28 pontos de chamada HTTP do `tiny.go` usam `DoRequestWithRetry`** (`tiny.go:1616, 1754, 1841, 2184`; os outros 24 usam `t.DoRequest` cru). **`ReserveStock`, `ReverseStockReservation`, `LaunchOrderStock` e `CreateOrder` não tratam 429 de forma alguma.** O caminho que mais precisa é o que menos protege. |
| Um único limiar adaptativo | **Insuficiente.** São **dois** baldes com janelas de 1 s e 60 s. Um limiter de janela única sempre erra um dos dois. |

### Aritmética de deadline, revisada

`[CÓDIGO tiny.go:91]` HTTP client = 30 s. `[CÓDIGO events/types.go:272-275]` asynq `order.paid` = **90 s**.
Um pedido pago faz **N + 6 a N + 11 chamadas sequenciais** `[CÓDIGO: contagem dos call sites — ver AUDIT §2]`: N estornos + 1-2
contato + 1-2 `/formas-envio` + **1 a 5** `/formas-recebimento` + `POST /pedidos` + marcador + situação +
lançamento.

Latências reais `[EMPÍRICO 11/07: dur_ms em 333/333]` — leitura **250–300 ms**, escrita **800–900 ms**:

| método | endpoint | n | p50 | p95 | max |
|---|---|---:|---:|---:|---:|
| GET | `/estoque/{id}` | 92 | 270 | 387 | 472 |
| GET | `/pedidos/{id}` | 8 | 248 | 314 | 330 |
| POST | `/pedidos` | 42 | **847** | 1076 | 1239 |
| POST | `/estoque/{id}` | 10 | **786** | 837 | 891 |
| POST | `.../lancar-estoque` | 20 | 814 | 907 | 922 |
| POST | `.../estornar-estoque` | 37 | 472 | 971 | 1032 |
| PUT | `.../itens` | 12 | **893** | 1009 | **1823** |
| PUT | `.../situacao` | 34 | 763 | 902 | 1678 |

Com N=15 e p50 de 800 ms: **~15 s no melhor caso, >30 s no p95** — cabe nos 90 s **se e somente se nenhum 429
entrar**. Mas **N+11 chamadas já são ~40% do balde sustentado inteiro de um minuto**, e o pior ponto é logo depois
do `POST /pedidos`: pedido criado no Tiny, `external_order_id` não gravado, marcador não aplicado, aprovação não
executada = os pedidos órfãos de 16/08 e 25/08.

🔴 **Amplificação (auto-DDoS), e agora com número:** cada `POST /estoque` nosso dispara um webhook `tipo:estoque` do
Tiny, que o nosso `HandleTiny` responde com `GET /produtos/{id}` (+ `GET /estoque/{id}` se `use_available_stock`)
`[EMPÍRICO 11/07 + CÓDIGO webhook_handler.go:680]`. **O estorno de N reservas gera ≥N leituras extras na mesma
janela de 60 s em que a finalização precisa de cota.** Com N=15 isso é 30 chamadas — **o balde sustentado inteiro**.

## 6.2 Catálogo de erros com corpo exato

### Todas as respostas ≥400 da bateria de 11/07 — extraídas do `actions.jsonl`

52 erros em 333 requests = **15,6%** `[EMPÍRICO 11/07: agrupamento programático]`:

| n | status | método + endpoint | corpo **exato** |
|---:|---|---|---|
| **30** | 500 | `GET /pedidos?marcadores[]=<tag>` | `{"mensagem": "Internal Server Error"}` |
| **13** | 409 | `POST /pedidos` | `{"mensagem": "Esse registro já existe"}` |
| **3** | 400 | `POST /pedidos/{id}/lancar-estoque` | `{"mensagem": "Estoque já lançado."}` |
| **2** | 400 | `PUT /pedidos/{id}/itens` | `{"mensagem": "Ocorreram erros de validação", "detalhes": [{"campo": "pedido.motivosBloqueio[0]", "mensagem": "estoque lançado"}]}` |
| **2** | 429 | `POST /pedidos` | *(corpo vazio — `resp: null`)* |
| **1** | 429 | `GET /pedidos?numeroPedidoEcommerce=` | *(corpo vazio)* |
| **1** | 429 | `GET /pedidos?marcadores[]=` | *(corpo vazio)* |

**Nenhum outro código de erro foi observado** — sem 401, 403, 404, 422, e sem 500 fora do caso de marcadores.

### Erros de produção que a bateria NÃO reproduziu `[EMPÍRICO prod]`

| status | endpoint | mensagem |
|---|---|---|
| 400 | `POST /pedidos/{id}/lancar-estoque` | `"saldo em estoque de um ou mais produtos é insuficiente"` |
| 400 | `POST /pedidos` | `transportador.formaEnvio.id: "Forma de envio não habilitada"` |
| 400 | `POST /pedidos` (parcela) ⚠️ | `pagamento.parcelas[0].meioPagamento.id: "Meio de pagamento não encontrado"` — o corpo está transcrito em `[CÓDIGO tiny.go:1300-1310]`, **dentro do `CreateOrder`**; a redação anterior atribuía ao `PUT`. O comentário não registra o verbo, então `[ABERTO]` se o `PUT` também recusa |
| 400 | `PUT /pedidos/{id}` | `"Forma de recebimento da parcela diferente da forma de recebimento do pedido"` |
| 429 | `POST /pedidos`, `POST /marcadores`, `PUT /situacao` | *(rajadas durante finalização)* |
| 403 | `GET /depositos`, `GET /estoque/{id}/logs-movimentacao` | *(corpo não capturado — ver abaixo)* `[EMPÍRICO 25/08]` |

### As formas de corpo — só duas, mais uma não declarada

```json
// (1) simples
{"mensagem": "<texto>"}

// (2) validação — a forma canônica do 400 [SWAGGER: ErrorDTO]
{"mensagem": "Ocorreram erros de validação",
 "detalhes": [{"campo": "<caminho>", "mensagem": "<texto>"}]}

// (3) NÃO DECLARADO — motivosBloqueio, com campo em caminho INDEXADO
{"mensagem": "Ocorreram erros de validação",
 "detalhes": [{"campo": "pedido.motivosBloqueio[0]", "mensagem": "estoque lançado"}]}
```

⚠️ **`404 / 403 / 401 / 500 / 503 são declarados SEM `content` em 202/202 operações** `[SWAGGER]` — **a spec não
promete corpo JSON em erro que não seja 400.** Os 403 de hoje vieram com `Content-Type: application/json` mas o
corpo não foi capturado pela ferramenta `[EMPÍRICO 25/08]` — `[ABERTO]`, e a chamada que fecharia é trivial: repetir
o `GET /depositos` gravando o corpo cru do 403.

### 🔴 O que o catálogo mostra sobre o nosso tratamento de erro

`[CÓDIGO]` **Não existe taxonomia de erro do Tiny.** Só três discriminadores em todo o cliente: o status HTTP
espalhado, **três matchers por substring** (`"já lançado"` / `"formaenvio"` / `"insuficiente"`) e o sentinela
`ErrProvenUndelivered`. E **41 pontos mapeados** onde o erro do ERP vira só log e o fluxo segue — 13 dentro de
`tiny.go`, 28 nos orquestradores. Os mais caros:

| Ponto | Efeito |
|---|---|
| `tiny.go:1482-1488` | `AddOrderMarker` falha → `Warn` e **`CreateOrder` retorna SUCESSO**. Pedido sem âncora de recuperação — foi o caso de 2 pedidos pagos perdidos em 16/08 |
| `tiny.go:1493-1500` | `ApproveOrder` falha → `Warn` e sucesso. **Pedido nasce "Em aberto"**, lojista aprova na mão |
| `tiny.go:476-495` | `saldoDisponivel` falha → **cai no saldo FÍSICO**, reofertando estoque reservado (incidente `834962410`, 22/08) |
| `integration/service.go:6266-6273` | **refresh de token falha → `Warn` e segue com credencial expirada** |
| `erp/reconciliation.go:99-108` | falha de leitura conta como `Skipped++` e **o produto some do relatório** |

**Sem catálogo, esses 41 pontos viram 41 decisões ad-hoc de novo na Fase 1.** O catálogo acima é o começo dele.

## 6.3 Paginação, datas e dinheiro

### Paginação `[SWAGGER]`

Envelope padrão: `{"itens":[…], "paginacao":{"limit":N,"offset":N,"total":N}}` — chaves **`itens`** (não
`items`/`data`) e **`paginacao`**. `limit` default **100**, **sem `maximum` declarado**; `offset` default 0.
**Offset puro — sem cursor, sem `hasMore`, sem `nextOffset`.** `orderBy` só diz a **direção** (`asc`/`desc`), não
o campo.

Confirmado hoje `[EMPÍRICO 25/08]`: `GET /pedidos?limit=1` → `{"itens":[],"paginacao":{"limit":1,"offset":0,
"total":0}}`; `GET /produtos?limit=1` → `"paginacao":{"limit":1,"offset":0,"total":21}`.

**Exceções ao envelope — array cru na raiz** `[SWAGGER — recontado nesta revisão: são **15** GETs, não 6]`:
`/depositos` · `/crm/estagios` · `/orcamentos/modelos` · `/produtos/{id}/anexos` · `/produtos/{id}/kit` ·
`/pedidos/{id}/marcadores` · `/notas/{id}/marcadores` · `/orcamentos/{id}/marcadores` ·
`/ordem-compra/{id}/marcadores` · `/ordem-servico/{id}/marcadores` · `/crm/assuntos/{id}/marcadores` ·
`/contas-receber/{id}/marcadores` · `/contas-pagar/{id}/marcadores` · `/contas-receber/{id}/recebimentos` ·
`/contas-pagar/{id}/recebimentos`. **O padrão é: toda sub-coleção de marcadores e de recebimentos é array cru.**
E `logs-movimentacao` devolve `PaginatedResultModel` **direto** (§4.3).

`[ABERTO]` **`limit` máximo.** Testar 200/500/1000/5000: erro 400 ou clamp silencioso? Importa porque a única
alternativa à ausência de busca em lote é paginar grande.

`[ABERTO]` `total` é exato ou estimado? É estável durante paginação concorrente?

### Datas — **quatro** formatos convivem, nenhum declara `format` nem timezone

`[SWAGGER]` `format:date` = 0 ocorrências · `format:date-time` = 0. Contagem de `example`:
**`YYYY-MM-DD` 109×** · **`YYYY-MM-DD HH:MM:SS` 19×** · **`DD/MM/YYYY` 2×** (`POST /contas-receber/{id}/baixar`).
E `[EMPÍRICO 11/07]` o **webhook** usa `DD/MM/YYYY` — quarto contexto.

Exemplos concretos do mesmo recurso `[EMPÍRICO 11/07: T5]`: aceita `"2026-07-11"` no PUT, devolve
`"2026-07-11 00:00:00"` na parcela e `"2026-07-11"` na raiz.

⚠️ `[EMPÍRICO prod / CÓDIGO tiny.go:35-39]` — **timezone já mordeu**: *"Tiny é um ERP brasileiro e interpreta
campos `data` contra o horário local de São Paulo. Enviar UTC fez pedidos criados de madrugada caírem no dia
seguinte na perspectiva do Tiny, saindo do filtro 'últimos 30 dias' do lojista"*. Nenhum offset é declarado em
lugar nenhum do swagger.

⚠️ **Data ausente vem como string vazia, não `null`** `[EMPÍRICO 11/07: GET /pedidos/{id}]`: `"dataEntrega":""`.
Um `time.Parse("")` retorna erro; um parser que trate erro como "campo ausente" e outro que trate como falha
produzem comportamentos diferentes no mesmo pedido. **Vale auditar todos os parses de data do `tiny.go` contra
`""` antes de confiar em qualquer reconciliação por `dataAtualizacao`.**

`[ABERTO]` formato aceito em `dataInicial`/`dataFinal`/`dataAtualizacao` (o swagger é `type: string` puro, sem
`format`, sem `pattern`, **sem `example`**); intervalo inclusivo nas duas pontas?; `dataAtualizacao` é ponto único
ou janela? **Crítico para reconciliação incremental.**

### Dinheiro

**Sempre `number/float`** — 224 ocorrências, o formato mais comum do arquivo. Nunca string, nunca centavos
`[SWAGGER]`. ⚠️ `format: float` é IEEE-754 simples — **nem float nem double são exatos para dinheiro**.
⚠️ **Exceção**: `ListagemPedidoModelResponse.valor` é **string** (`"10"`, `""` para grade vazia) contra
`ObterPedidoModelResponse.valorTotalPedido` float — nome **e** tipo diferentes para a mesma grandeza
`[SWAGGER + EMPÍRICO 11/07]`.

### Uma nota que muda expectativa

`[EMPÍRICO 25/08: diff programático]` os schemas de **resposta** de Pedidos batem campo a campo com a realidade
(`swagger-only: []`, `real-only: []`, no detalhe e na listagem). **Este swagger é confiável na resposta e
não-confiável no request.** Nenhum relatório anterior fazia essa distinção. Consequência prática:
**parsers de resposta podem ser gerados; validação de request, não.**

## 6.4 Webhooks

### Gestão programática: NÃO EXISTE

`[SWAGGER]` `webhook` = 0 · `callback` = 0 · `notifica` = 0 · sem chave `webhooks` no documento (é OpenAPI 3.0.0) ·
sem `components.callbacks`. **Nenhum endpoint para registrar, listar, testar ou remover assinatura; nenhum schema
de payload; nenhuma menção a HMAC.** `[EMPÍRICO: grep em providers/erp/]` nada no nosso código registra webhook
tampouco. **O cadastro é 100% manual no painel do lojista** — confirmado por três vias independentes.

`[EMPÍRICO prod]` Consequência já registrada: a morte de um webhook em produção é **extensão/plano/toggle no painel
do Tiny**, não bug nosso; e a URL do túnel muda a cada start do `cloudflared`, então **cada restart do túnel exige
re-cadastro manual**.

### Envelope — idêntico em 75/75 `[EMPÍRICO 11/07]`

```json
{"versao": "1.0.1", "cnpj": "<14 dígitos, SEM máscara>", "tipo": "…", "dados": {…}}
```

**Exatamente 4 chaves de topo.** As 75 linhas têm exatamente `{cnpj, dados, tipo, versao}`.
⚠️ **O `cnpj` vem sem pontuação** (`"59573950000158"`), enquanto `GET /info` devolve **com**
(`"59.573.950/0001-58"`) `[EMPÍRICO 25/08: corpo do /info transcrito abaixo]`. **Quem casar conta por CNPJ precisa
normalizar.**

```json
// GET /info hoje  [EMPÍRICO 25/08]
{"razaoSocial":"ADABYTE LTDA","cpfCnpj":"59.573.950/0001-58","fantasia":"",
 "enderecoEmpresa":{"endereco":"AVENIDA PAULISTA","numero":"777","complemento":"ANDAR 15            ",
  "bairro":"BELA VISTA","municipio":"São Paulo","cep":"01.311-914","uf":"SP","pais":""},
 "fone":"","email":"eng@livecart.com.br","inscricaoEstadual":"","regimeTributario":1}
```

### Tipos observados — exatamente 3 `[EMPÍRICO 11/07]`

| `tipo` | n | Disparado por |
|---|---:|---|
| `estoque` | 30 | qualquer mudança de saldo (`POST /estoque`, `lancar-estoque`, `estornar-estoque`) |
| `inclusao_pedido` | 26 | `POST /pedidos` |
| `atualizacao_pedido` | 19 | `PUT /pedidos/{id}`, `PUT .../itens`, `PUT .../situacao` |

**Nunca observados**: `produto`, `nota_fiscal` — apesar de `[CÓDIGO webhook_handler.go:610-623]` parsear
`idPedido`, `idNotaFiscal`, `chaveAcesso` e `situacao`. `[ABERTO]` ou vêm de outra fonte, ou são especulação no
código.

### Payload real — `inclusao_pedido` `[EMPÍRICO 11/07: webhooks.jsonl, linha 1]`

```json
{"ts":"2026-07-11T17:38:07.206200457Z",
 "path":"/api/webhooks/tiny/11111111-2222-3333-4444-555555555555",
 "body":{
   "versao":"1.0.1","cnpj":"59573950000158","tipo":"inclusao_pedido",
   "dados":{"id":"358126298","numero":"1","data":"11/07/2026","idPedidoEcommerce":"",
            "codigoSituacao":"aberto","descricaoSituacao":"Em aberto",
            "idContato":"895591553","idNotaFiscal":"0","nomeEcommerce":"",
            "cliente":{"nome":"LIVECART SANDBOX CLIENTE — NAO USAR","cpfCnpj":null}}},
 "remote":"127.0.0.1:48094"}
```

### Payload real — `atualizacao_pedido` `[EMPÍRICO 11/07]`

```json
{"versao":"1.0.1","cnpj":"59573950000158","tipo":"atualizacao_pedido",
 "dados":{"id":"358126434","numero":"2","data":"11/07/2026","idPedidoEcommerce":"",
          "codigoSituacao":"aprovado","descricaoSituacao":"Aprovado",
          "idContato":"895591553","idNotaFiscal":"0","nomeEcommerce":"",
          "cliente":{"nome":"LIVECART SANDBOX CLIENTE — NAO USAR","cpfCnpj":null}}}
```

**`inclusao_pedido` e `atualizacao_pedido` têm o MESMO conjunto de 10 campos em `dados`** (26/26 e 19/19).

### Payload real — `estoque` `[EMPÍRICO 11/07]`

```json
{"versao":"1.0.1","cnpj":"59573950000158","tipo":"estoque",
 "dados":{"idProduto":357281337,"sku":"",
          "nome":"Console PlayStation® 5 Slim Edição Digital 825 GB Branco - Sony","saldo":8}}
```

`dados` tem **exatamente 4 campos em 30/30**: `{idProduto, nome, saldo, sku}`.

🔴 **Não há `reservado` nem `disponivel` no webhook de estoque. O único número que chega é o saldo FÍSICO.** Isso
responde diretamente a dúvida escrita em `[CÓDIGO webhook_handler.go:637-645]` (*"o `saldo` deste payload é parseado
e nunca usado, então ninguém sabe qual dos dois ele é"*): **é o físico** — o webhook mandava 8, 7, 6, 5, 3, 2, 1, 0
sempre casando com `saldo`, enquanto `disponivel` era 0 constante.
**Consequência para o Caminho A:** numa conta com reserva ativa, **o webhook NUNCA informa o disponível — sempre
exige um `GET /estoque` extra por produto.** É esse GET que produz a tempestade de 429 (§6.1).

### Tipagem — pegadinhas em toda linha `[EMPÍRICO 11/07]`

- `dados.id` e `dados.numero` são **STRING** no webhook (`"358126298"`, `"1"`), **INTEIRO** no `GET /pedidos/{id}`.
- `dados.idProduto` é **INTEIRO** no webhook de estoque (enquanto `id` é string no de pedido).
- `dados.data` é **`DD/MM/YYYY`**.
- `cliente.cpfCnpj` veio **`null`**; o `GET /pedidos/{id}` devolve `""` para o mesmo contato.
- **`codigoSituacao` é textual, não numérico.** Observados: `"aberto"`/"Em aberto" (37), `"aprovado"`/"Aprovado"
  (5), `"cancelado"`/"Cancelado" (3). **Não há mapa oficial entre esses slugs e os inteiros 0/3/2 do REST** — o
  mapeamento é `[ANÁLISE sólida, correlação 1:1 em 45/45 casos com os `PUT /situacao` que os provocaram]`. Os slugs
  de `4/1/7/5/6/8/9` **não foram observados** — `[ABERTO]`.
- **O webhook não carrega itens, valores, frete nem endereço.** Só identidade + situação.

### Assinatura / HMAC — a resposta honesta

**Não sabemos se existe assinatura, porque a bateria de 11/07 não gravou headers.** O `webhooks.jsonl` guardou só
`{ts, path, body, remote}` — **os headers foram descartados pelo harness**. Esse arquivo, portanto, **não pode
provar nem negar HMAC**.

O que sustenta a afirmação hoje é apenas: (a) `[CÓDIGO webhook_handler.go:670]` `SignatureValid: true, // Tiny
doesn't use signatures`, rodando em produção há meses sem verificar nada; e (b) `[SWAGGER]` não há seção `webhooks`
nem schema de assinatura. **Nenhuma das duas é medição.**

✅ **A ponte nova grava headers.** `[EMPÍRICO 25/08]` o `tiny-lab` já capturou 7 eventos **com headers completos**,
inclusive um teste ponta a ponta pelo túnel (`Cf-Ray: a30c5c9c29c2c8ae-GRU`, `Cf-Worker: trycloudflare.com`,
`Cf-Connecting-Ip: 170.82.242.126`). **Uma sessão de webhook real fecha a pergunta.**

⚠️ **A autenticação do nosso endpoint é o UUID secreto na URL — e mais nada.** Sem HMAC, sem allowlist de IP, e
`[EMPÍRICO 25/08: grep]` **a única ocorrência de `cnpj` em `webhook_handler.go` é um comentário** (`:602`).
`HandleTiny` resolve a loja **só pelo `storeId` da URL**, e `:592` dispara `RecordWebhookPing` **antes de qualquer
validação** — ou seja, **o único sinal de "o webhook manual está vivo" fica verde com tráfego de qualquer origem,
inclusive de outra conta.** É diagnóstico envenenado no meio de uma live, e é o mesmo padrão do bug cross-plataforma
do Pagar.me. **Como fechar, barato:** gravar o `cpfCnpj` do `GET /info` no `integrations.metadata` no momento do
OAuth e comparar com `dados.cnpj` antes do ping.

### 🔴 Ordem, entrega e a corrida com a resposta HTTP `[EMPÍRICO 11/07]`

**Não há NENHUM campo de ordem** em 75/75: nem `seq`, nem id de evento, nem timestamp de emissão. O
`versao:"1.0.1"` é a versão do schema, não do registro.

**Prova direta do defeito `erp_ack_seq`** — três webhooks do mesmo produto em **253 ms**, com valores **absolutos**:

```
17:57:56.986  saldo=6
17:57:57.131  saldo=5
17:57:57.239  saldo=3
```

Processados em paralelo (`go func()` no nosso handler), **o espelho fica em 6 quando a verdade é 3**. No produto B
foi pior: **6 webhooks em 2,9 s**, incluindo o valor **9 que nenhum GET jamais observou** (estado transiente).

**Entrega não é garantida:** 27 pedidos criados com 201 → **26** `inclusao_pedido`. O ausente é `358130585`,
**exatamente o segundo dos dois criados concorrentemente em C5**, com o receptor comprovadamente vivo depois.
**A ausência de webhook NÃO prova que a operação não entrou.**

🔴 **O webhook chega ANTES da resposta HTTP.** Casando 26 pares `POST /pedidos` × `inclusao_pedido` pelo `id`:

| métrica | lag (webhook − fim da resposta HTTP) |
|---|---|
| mínimo | **−0,084 s** |
| **mediana** | **−0,023 s** |
| máximo | +3,79 s (caso único, sob rajada de 429) |
| negativo em | **18 de 26 casos** |

**O Tiny dispara de dentro da transação.** Consequência arquitetural direta: **é impossível garantir que o
`external_order_id` esteja gravado no nosso banco quando o webhook chegar.** Todo handler de webhook de pedido tem
de tolerar "não conheço esse id ainda". É a explicação do pedido órfão `847978450` (#1087) em produção.

`[ABERTO]` **A regra dos "20 não-200".** `[CÓDIGO webhook_handler.go:594-595]` afirma *"after 20 consecutive
non-200 responses, Tiny automatically removes the webhook URL"* — **não aparece na doc pública e nunca foi
verificada.** Nunca devolvemos não-200 de propósito.

⚠️ `[EMPÍRICO prod / CÓDIGO webhook_handler.go:66-94]` **O painel do Tiny SONDA a URL antes de aceitar o cadastro**,
com métodos que não são POST. Registradas só como POST, as rotas devolviam 405 e o painel recusava com *"Não foi
possível acessar a URL"*. Por isso registramos `GET/HEAD/OPTIONS/PUT/PATCH/DELETE` devolvendo **200 JSON**
(`{"ok":true}` — **não `text/plain`**, um validador que faz parse engasga). **Qualquer ponte nova tem de replicar
isso.**

## 6.5 Autenticação

`[SWAGGER]` o bloco inteiro de segurança é `{"bearerAuth":{"type":"http","scheme":"Bearer"}}` — **zero ocorrências
de oauth/scope/tokenUrl/refresh_token**. (`scheme` com B maiúsculo, fora do canônico.)

**Na prática é Keycloak, realm `tiny`** `[CÓDIGO tiny.go:43-44]`, e foi exercitado hoje `[EMPÍRICO 25/08]`:

```
POST https://accounts.tiny.com.br/realms/tiny/protocol/openid-connect/token
grant_type=authorization_code  →  200 em 252 ms
expires_in = 14400 (4 h)   scope = "openid email offline_access"   aud = "tiny-api"
```

Três fatos novos `[EMPÍRICO 25/08]`:
1. **O fluxo `authorization_code` funcionou com PKCE S256.** O backend de produção **não usa PKCE**.
   `[CÓDIGO integration/service.go:930]` usa `state := storeID` cru e `:1196` faz `storeID := input.State` **sem
   validação nenhuma** — é CSRF-able e vaza o UUID da loja na URL do browser. **A tabela `oauth_states` já existe**
   (migration `000027`, com coluna `code_verifier`) e o Melhor Envio já usa o desenho certo
   (`melhor_envio_oauth.go:44-82`). O caminho está pavimentado.
2. **`expires_in` veio 14400 (4 h) de verdade.** `[CÓDIGO]` o código **chuta 4 h quando `expires_in` vem 0 — em
   dois lugares, sempre com `Warn`**, ou seja alguém já viu o campo vir zerado. `[ABERTO]` em que condição.
3. **O JWT carrega 48 roles em `resource_access.tiny-api`**, incluindo `depositos-leitura` e `estoque-leitura` —
   o que **prova que os 403 de hoje são de conta/plano, não de escopo do app**.

`[ABERTO]` rotação do refresh token; se o corpo do 401 distingue expirado × revogado × escopo faltando.

⚠️ `[CÓDIGO providers/erp/tiny.go:147-152]` **`RefreshToken` monta o form com `fmt.Sprintf`, sem escape**, enquanto
o callback (`service.go:1236`) usa `url.Values` com o comentário explícito *"for proper URL encoding"*. **Um
`client_secret` contendo `+`, `&` ou `=` corrompe o refresh** — e quando falha, `service.go:6315` marca a
integração como `error`.

---

# 7. Tabela de cobertura da Fase 0

Uma linha por pergunta. **RESPONDIDO** = há medição · **PARCIAL** = medido em condição que não transfere, ou com
ressalva · **ABERTO** = não sabemos.

## 7.1 POST /pedidos — obrigatoriedade e payload

| # | Pergunta | Status | Evidência | Chamada que fecharia |
|---|---|---|---|---|
| A1 | Quais campos o Tiny **de fato** exige? | **ABERTO** | `[SWAGGER]` zero `required` em toda a cadeia; a mecânica existe e não foi aplicada | `POST /pedidos {}` → catalogar `detalhes[].campo`; remover um a um `idContato`, `data`, `itens`, `itens[].quantidade`, `itens[].valorUnitario` |
| A2 | Qual o mínimo que funciona? | **RESPONDIDO** | `[EMPÍRICO 11/07 #9]` `{data, idContato, itens[{produto{id},quantidade,valorUnitario}]}` → 201 | — |
| A3 | `data` aceita ISO com hora/timezone? | **ABERTO** | — | POST com `"2026-08-25T14:00:00Z"` |
| A4 | **`situacao:3` no POST nasce Aprovada e dispara os mesmos efeitos?** | **ABERTO** | `[SWAGGER]` enum no corpo; `[EMPÍRICO 11/07]` `situacao` **nunca foi enviada** em 42 POSTs | `POST /pedidos {…,"situacao":3}` → `GET /pedidos/{id}` + `GET /estoque/{id}` |
| A5 | `valorFrete`/`valorDesconto` alteram `valorTotalPedido`? | **ABERTO** | `[SWAGGER]` sem `description`; a fórmula existe só p/ orçamentos | criar com frete e comparar `valorTotalPedido` × `valorTotalProdutos` |
| A6 | Tabela de `tipoPagamento` / `codigoBandeira` | **ABERTO** | `[SWAGGER]` integers sem enum, sem tabela; `MeioPagamentoResponseModel` é órfão → **não há endpoint que liste** | nenhuma chamada resolve — pedir ao fornecedor |
| A7 | Endereço + frete no payload funcionam? | **ABERTO** | `[EMPÍRICO 11/07]` **nenhum dos 42 pedidos teve `enderecoEntrega`/`transportador`/`valorFrete`** | 1 POST com o bloco completo + GET |
| A8 | `fretePorConta:"S"` serve p/ retirada? | **ABERTO** | `[CÓDIGO tiny.go:1209]` fixamos `"D"` | POST com `"S"`, sem `formaEnvio`, com e sem `valorFrete` |
| A9 | `formaEnvio` não habilitada rejeita o pedido inteiro? | **RESPONDIDO** | `[EMPÍRICO prod 16/08]` `transportador.formaEnvio.id: "Forma de envio não habilitada"` | — (falta só validar que `?situacao=1` evita) |
| A10 | `meioPagamento.id` — qual namespace? | **PARCIAL** | `[CÓDIGO tiny.go:1300-1310]` não é o de `/formas-pagamento`; desativado | `[ABERTO]` sem endpoint que liste |
| A11 | Validação `formaRecebimento` parcela × pedido | **RESPONDIDO** | `[EMPÍRICO 11/07 E2E]` 400 com texto exato | — |
| A12 | O default id=1 "Múltiplas" explica esse 400? | **ABERTO** `[ANÁLISE]` | `[EMPÍRICO 11/07]` o Tiny grava `formaPagamento/formaRecebimento = {1,"Múltiplas"}` sozinho | POST enviando `pagamento.formaRecebimento` explícito |

## 7.2 Busca, listagem e paginação

| # | Pergunta | Status | Evidência | Chamada que fecharia |
|---|---|---|---|---|
| B1 | Como achar pelo número da tela? | **RESPONDIDO** | `[SWAGGER]` `GET /pedidos?numero=N` → `itens[0].id` → `GET /pedidos/{id}`; **não existe path alternativo** (17/17 verificados) | — |
| B2 | `numero` é exato ou parcial? | **ABERTO** | — | `?numero=118` num conjunto com 118 e 1186 |
| B3 | O número é único por conta? | **PARCIAL** | `[EMPÍRICO prod #1087]` o número 27187 foi **reciclado** 1 h após exclusão | criar/excluir/criar |
| B4 | Sem resultado: 200 ou 404? | **RESPONDIDO** | `[EMPÍRICO 25/08]` `200 {"itens":[],"paginacao":{…,"total":0}}` | — |
| B5 | `numeroOrdemCompra` serve de filtro? | **RESPONDIDO — NÃO** | `[EMPÍRICO 11/07 T8]` grava e persiste, mas não há param `[SWAGGER]` | — |
| B6 | `numeroPedidoEcommerce` serve de âncora? | **RESPONDIDO — MORTO** | `[EMPÍRICO 11/07 T8]` aceito e descartado (GET devolve `""`); filtro `total:0` em 31 tentativas | — |
| B7 | Marcadores como âncora | **RESPONDIDO** | `[EMPÍRICO 11/07 T8]` `?marcadores=` 200 na 1ª; `?marcadores[]=` **500 em 30/30** | — |
| B8 | Read-after-write do marcador | **PARCIAL** | ⚠️ o "~300 ms" do código é **má leitura** do `dur_ms`; o real provado é **≤112 s** | sondar `?marcadores=` a 500 ms após o POST |
| B9 | `marcadores=a&marcadores=b` é AND ou OR? exato ou parcial? | **ABERTO** | — | 2 tags no mesmo pedido + consulta com as duas |
| B10 | `limit` máximo | **ABERTO** | `[SWAGGER]` default 100, **sem `maximum`** | testar 200/500/1000/5000 |
| B11 | Formato/timezone de `dataInicial`/`dataAtualizacao` | **ABERTO** | `[SWAGGER]` `type:string` puro, sem `format`/`pattern`/`example` | 4 formatos × 1 chamada cada |
| B12 | `dataAtualizacao` é ponto ou janela? | **ABERTO** | — | com e sem `dataInicial/dataFinal` |
| B13 | A listagem traz `itens`/`valorFrete`/`marcadores`? | **RESPONDIDO — NÃO** | `[SWAGGER]` verificado campo a campo contra o real | — |

## 7.3 PUT /itens

| # | Pergunta | Status | Evidência | Chamada que fecharia |
|---|---|---|---|---|
| C1 | Substitui a grade inteira? | **PARCIAL** | `[SWAGGER]` afirmado **duas vezes**, literal; `[EMPÍRICO 11/07 T6-e]` `{"itens":[]}` → 204 e pedido sem itens (um merge seria no-op). **Remoção seletiva nunca foi testada** | grade de 2 linhas → `PUT /itens` com 1 → `GET` |
| C2 | `itens: []` apaga ou 400? | **RESPONDIDO** | `[EMPÍRICO 11/07 T6-e]` **204**; pedido fica com `"valor":""` | — |
| C3 | Duas linhas do mesmo `produto.id` | **ABERTO** | — | `PUT` com a duplicata |
| C4 | **O que acontece com `valorFrete`/`valorDesconto`?** | **ABERTO — decide a arquitetura** | `[SWAGGER]` mudo; nunca testado | criar com frete → `PUT /itens` → ler `valorFrete` |
| C5 | E com as parcelas? | **PARCIAL** | `[SWAGGER]` "recalculados"; `[EMPÍRICO 11/07 T6-d]` 20→10, **sem GET intermediário** | 3 parcelas + total mudando; 0 parcelas |
| C6 | Bloqueio por estoque lançado + shape do 400 | **RESPONDIDO** | `[EMPÍRICO 11/07 T6-b]` corpo exato capturado; `motivosBloqueio` não existe no `ErrorDTO` | — |
| C7 | Aceito em pedido Aprovado? | **RESPONDIDO** | `[EMPÍRICO 11/07 T6-c]` 204 | — |
| C8 | **Aceito em pedido apenas RESERVADO (conta A)?** | 🔴 **ABERTO — a suposição do fluxo alvo** | nunca testado; toda a base é conta B | a sequência de 10 chamadas de §2.4 |
| C9 | Existe ETag / `If-Match` / lock? | **RESPONDIDO — NÃO** | `[SWAGGER]` nenhum; `[EMPÍRICO 11/07 T11/C2]` last-write-wins, ambos 204 | — |
| C10 | `infoAdicional` sobrevive se omitido? | **ABERTO** | — | PUT sem o campo + GET |

## 7.4 Situação

| # | Pergunta | Status | Evidência | Chamada que fecharia |
|---|---|---|---|---|
| D1 | Enum real | **RESPONDIDO — bate 100%** | `[SWAGGER]` `[8,0,3,4,1,7,5,6,2,9]`, idêntico em 4 pontos | — |
| D2 | Transições proibidas | **ABERTO** | `[SWAGGER]` zero; `[EMPÍRICO 11/07]` só `0→3`, `0→2`, `3→2`; **nunca houve um 400** | matriz 10×10, prioridade `0→3, 3→1, 1→2, 2→3, 3→4, 4→7, 7→5, 5→6, 6→0, 3→3` |
| D3 | Cancelar devolve estoque? | **RESPONDIDO — NÃO** | `[EMPÍRICO 11/07 T7]` saldo 1→1 | — |
| D4 | `estornar-estoque` em pedido cancelado? | **RESPONDIDO — SIM** | `[EMPÍRICO 11/07 T7]` 204, saldo 1→2 | — |
| D5 | Ordem correta do cancelamento | **PARCIAL — ambígua no nosso código** | `tiny.go:2149-2153` × `tiny.go:2046` × `order_lifecycle.go:644-647` | decidir + escrever num lugar só |
| D6 | Cancelar estorna **contas**? `→1 Faturada` lança contas? | **ABERTO** | nada no swagger, nunca chamado | `lancar-contas` → `situacao=2` → `?idVenda=` |
| D7 | Reaprovar é idempotente? | **RESPONDIDO — SIM** | `[CÓDIGO tiny.go:1927-1931]` + prod | — |
| D8 | Corrida cancelar ∥ lançar | **RESPONDIDO — bug do Tiny** | `[EMPÍRICO 11/07 T11/C4]` ambos 204, cancelado **com** estoque lançado | — |

## 7.5 Estoque

| # | Pergunta | Status | Evidência | Chamada que fecharia |
|---|---|---|---|---|
| E1 | Campos reais do `GET /estoque` | **RESPONDIDO** | `[EMPÍRICO 25/08 + 11/07 + prod]` corpo transcrito; bate com o swagger | — |
| E2 | Semântica de `saldo`/`reservado`/`disponivel` | **RESPONDIDO** | `[EMPÍRICO]` físico / reserva de documento Tiny / vendável (pode ser negativo) | — |
| E3 | **`possuiReserva` — a conta é A ou B?** | 🔴 **ABERTO** | `[SWAGGER]` o campo existe e é o detector; **403 na ADABYTE hoje**; `grep` no repo = 0 | `GET /depositos` numa conta com o módulo ativo |
| E4 | `logs-movimentacao` — corpo real | 🔴 **ABERTO** | `[SWAGGER]` 200 sem array de itens; **403 na ADABYTE hoje** | `POST /estoque` com key → `GET …/logs-movimentacao?…` cru |
| E5 | `observacao` do log ecoa `observacoes` do POST? | **ABERTO** | ⚠️ assimetria de nome (plural × singular) | idem E4 |
| E6 | `B`/`E`/`S` — delta ou absoluto | **RESPONDIDO** | `[EMPÍRICO 11/07 T4]` `465 --B10--> 10` | — |
| E7 | `maxLength` de `observacoes` | **ABERTO** | `[SWAGGER]` 0 ocorrências de `maxLength` no arquivo; `[EMPÍRICO prod]` ≥85 chars passa | POST 100/255/256/1000 e **ler de volta pelo log** |
| E8 | Dedup do `POST /estoque` | **RESPONDIDO — NÃO EXISTE** | `[EMPÍRICO 11/07 T10]` 2 corpos idênticos → 2 `idLancamento`, saldo 2× | — |
| E9 | `lancar-estoque` 2× | **RESPONDIDO** | `[EMPÍRICO 11/07 T2]` 400 `"Estoque já lançado."`, saldo intacto; atômico sob concorrência | — |
| E10 | `estornar-estoque` 2× / sem lançamento | **RESPONDIDO** | `[EMPÍRICO 11/07 T2]` 204, no-op, não infla | — |
| E11 | `deposito` omitido → qual? | **ABERTO** | **toda a base empírica é MONO-DEPÓSITO** | POST com e sem `deposito.id` + `GET /estoque.depositos[]` |
| E12 | Formato de `data` no POST | **ABERTO** | único campo de data do arquivo **sem `example` e sem `format`** | POST retroativo |
| E13 | Quando o Tiny lança sozinho | **ABERTO — config de conta** | `[EMPÍRICO 11/07 T1]` só manual × `[EMPÍRICO prod]` "already launched by Tiny automatically" | comparar contas + achar o toggle |
| E14 | Saldo negativo é permitido? | **ABERTO — config de conta** | `[EMPÍRICO 11/07 T3-r2]` −1 sem erro × `[EMPÍRICO prod #1186]` 400 "insuficiente" | idem |
| E15 | Kits, variações, `controlar:false`, `sobEncomenda` | **ABERTO** | ambos os produtos da bateria são `tipoVariacao:"N"` | 1 pedido com kit, 1 com variação |

## 7.6 Contas / financeiro

| # | Pergunta | Status | Evidência | Chamada que fecharia |
|---|---|---|---|---|
| F1 | Shape de `lancar-contas`/`estornar-contas` | **RESPONDIDO** | `[SWAGGER]` sem body, 204, só `idPedido` | — |
| F2 | O que `lancar-contas` gera | **ABERTO** | swagger mudo; 204 não devolve ids | `lancar-contas` → `GET /contas-receber?idVenda={id}` |
| F3 | É idempotente? | **ABERTO** | — | chamar 2× e contar |
| F4 | `idVenda` == `idPedido`? | **ABERTO** | `[SWAGGER]` não afirma; existe `idNota` separado | idem F2 |
| F5 | `estornar-contas` com conta já baixada | **ABERTO** | desfazer-baixa **não existe** | baixar → estornar |
| F6 | Estorno no nível da CONTA | **RESPONDIDO — NÃO EXISTE** | `[SWAGGER]` 10 operações, nenhuma desfaz baixa; `PUT` aceita 5 campos, nenhum mexe em situação | — |
| F7 | Onde entra o frete | **RESPONDIDO** | `[SWAGGER]` `valorFrete` escalar + `transportador{}`; ambos só na criação | — |
| F8 | Frete entra no total? | **ABERTO** | fórmula existe só p/ orçamento | criar com frete e comparar |
| F9 | Efeito nas parcelas | **PARCIAL** | ver C5 | — |
| F10 | `pagamentosIntegrados` × `parcelas` | **ABERTO** | swagger não explica a relação | um pedido de cada |
| F11 | Enum de situação de conta | **RESPONDIDO** | `[SWAGGER]` `aberto/cancelada/pago/parcial/prevista/atrasadas/emissao` | — |
| F12 | `/formas-pagamento` aceita `nome`/`situacao`? | **RESPONDIDO no contrato, ABERTO no comportamento** | `[SWAGGER]` os 3 endpoints aceitam; `[CÓDIGO tiny.go:1733-1742]` diz que não e pagina 5× | `GET /formas-recebimento?nome=Pix&situacao=1` |
| F13 | Cadastro real da conta | **RESPONDIDO** | `[EMPÍRICO 25/08]` 11 formas, `situacao` como **string**, metade desabilitada | — |
| F14 | Retirada em loja (`tipoEntrega=6`) | **PARCIAL** | `[SWAGGER]` só no detalhe de `/formas-envio/{id}`, nunca na listagem | listar + N detalhes |

## 7.7 Operacional

| # | Pergunta | Status | Evidência | Chamada que fecharia |
|---|---|---|---|---|
| G1 | **Rate limit real** | **RESPONDIDO** | `[EMPÍRICO 25/08]` **2 baldes: 4/s e 30/60 s**; sem `Retry-After`; headers em toda resposta | — |
| G2 | Nome do header | **RESPONDIDO** | `X-Ratelimit-Limit/Remaining/Reset` | — |
| G3 | Corpo do 429 | **RESPONDIDO — VAZIO** | `[EMPÍRICO 11/07: 4/4]` | — |
| G4 | Rate limit por endpoint ou global? | **ABERTO** | a rajada de hoje foi num endpoint só | rajada alternando 3 endpoints |
| G5 | Catálogo de erros | **RESPONDIDO** | §6.2, 7 formas com corpo exato | — |
| G6 | Corpo de 401/403/404/500/503 | **ABERTO** | `[SWAGGER]` declarados **sem `content`** em 202/202; os 403 de hoje não tiveram corpo gravado | repetir `GET /depositos` gravando o corpo cru |
| G7 | Paginação | **RESPONDIDO** | `{itens, paginacao{limit,offset,total}}`, offset puro, sem cursor | — |
| G8 | Formatos de data | **RESPONDIDO — 4 convivem** | `[SWAGGER]` 109× `Y-m-d`, 19× com hora, 2× `d/m/Y`; webhook `DD/MM/YYYY` | — |
| G9 | Timezone | **ABERTO — e já mordeu** | `[EMPÍRICO prod]` UTC fez pedidos caírem no dia seguinte | criar às 23:50 BRT e ler `data` |
| G10 | Busca em lote | **RESPONDIDO — NÃO EXISTE** | `[SWAGGER]` varredura de todos os filtros de todas as listagens | — |
| G11 | `Idempotency-Key` | **RESPONDIDO — NÃO** | `[EMPÍRICO 25/08]` `idempot`=0; **nenhum parâmetro `in: header` no arquivo inteiro** | — |
| G12 | Dedup nativa do POST /pedidos | **RESPONDIDO — existe, por hash, NÃO atômica** | `[EMPÍRICO 11/07]` 13×409 + C5 com 2 duplicatas criadas | — |
| G13 | O 409 devolve o id do existente? | **RESPONDIDO — NÃO** | `[EMPÍRICO 11/07]` corpo é só `{"mensagem":…}` | — |
| G14 | Janela da dedup | **PARCIAL** | provada **≥21 min**; não se sabe se é a data, 24 h ou permanente; nem se cancelado bloqueia | POST → cancelar → POST → POST no dia seguinte |
| G15 | Webhooks: envelope e tipos | **RESPONDIDO** | `[EMPÍRICO 11/07: 75/75]` `{versao,cnpj,tipo,dados}`, 3 tipos, payloads transcritos | — |
| G16 | Webhook de estoque = físico? | **RESPONDIDO — SIM** | `[EMPÍRICO 11/07: 30/30]` só `{idProduto,sku,nome,saldo}` | — |
| G17 | **Assinatura / HMAC** | **ABERTO — honestamente** | a bateria de 11/07 **descartou headers**; a afirmação atual vem só do código e do silêncio do swagger. **A ponte nova grava headers** | uma sessão de webhook real com o `tiny-lab` |
| G18 | Ordem / sequência | **RESPONDIDO — NÃO HÁ** | `[EMPÍRICO 11/07: 75/75]` nenhum campo de ordem; 3 saldos absolutos em 253 ms | — |
| G19 | Entrega garantida? | **RESPONDIDO — NÃO** | 27 pedidos → 26 webhooks | — |
| G20 | Webhook chega antes da resposta? | **RESPONDIDO — SIM** | mediana **−23 ms**, negativo em 18/26 | — |
| G21 | Regra dos "20 não-200" | **ABERTO** | `[CÓDIGO webhook_handler.go:594-595]` afirma; não está na doc pública | devolver 500 propositalmente 21× |
| G22 | Webhooks `produto` e `nota_fiscal` existem? | **ABERTO** | nunca observados; o handler os parseia | provocar (emitir NFe, alterar produto) |
| G23 | Gestão programática de webhook | **RESPONDIDO — NÃO EXISTE** | `[SWAGGER]` `webhook`=0 em 127 paths | — |
| G24 | TTL do token / rotação do refresh | **PARCIAL** | `[EMPÍRICO 25/08]` `expires_in=14400` real; `[CÓDIGO]` chuta 4 h quando vem 0, em 2 lugares, com `Warn` | observar um refresh completo |
| G25 | NFe: `/notafiscal/{id}` existe? | **ABERTO — provável 404 silencioso** | `[CÓDIGO tiny.go:2636, :2665]` chama `/notafiscal/…`; `[SWAGGER]` **não há path com "notafiscal"** — os reais são `/notas/{idNota}` e `/notas/{idNota}/xml` | chamar os dois paths com uma NFe real; e conferir se `GET /pedidos/{id}.idNotaFiscal` vem preenchido |

---

# 8. A pauta da próxima bateria

**Pré-requisito único e bloqueante:** decidir **qual conta**. A ADABYTE serve para tudo que é **contrato** e
**não serve** para nada que exija módulo de reserva ou permissão de `/depositos` e `/logs-movimentacao` (403
comprovado hoje). Recomendação: **duas fases** — ADABYTE para contrato; uma janela combinada com um lojista real,
sobre um produto descartável de saldo alto, para os testes de regime.

**Regra de ouro da bateria:** gravar **request, response, status, duração E HEADERS** de cada chamada. O erro do
harness de 11/07 foi descartar headers — é a razão de ainda não sabermos se há HMAC no webhook. O `tiny-lab` já faz
isso (`.tiny-lab/audit.jsonl`).

**Guard de escrita:** `TINY_ENV=sandbox` + `cpfCnpj` do `GET /info` na allowlist `TINY_ALLOWED_CNPJ`, conferido a
cada execução. **A allowlist está VAZIA hoje — o primeiro ato da bateria é uma pessoa decidir e escrever o CNPJ
autorizado.** Não descobrir por tentativa. **Não existe sandbox do lado do fornecedor: toda escrita é numa conta
real.**

## 8.1 Ordem de execução — os cinco primeiros mudam a arquitetura

| # | Teste | Request exato | O que decide | Conta |
|---|---|---|---|---|
| **1** | **`possuiReserva`** | `GET /depositos` | **Caminho A vs B, por conta.** Vira gate de onboarding: detectamos por loja em vez de descobrir por falha silenciosa. Ler também `padrao`, `tipo`, `desconsideraSaldo` e **quantos depósitos** | ⚠️ **precisa de permissão** — ADABYTE dá 403 |
| **2** | **`logs-movimentacao`** | `POST /estoque/{id} {"tipo":"E","quantidade":1,"precoUnitario":10,"observacoes":"LAB-T-L1-<uuid>"}` → `GET /estoque/{id}/logs-movimentacao?dataInicio=D&dataFim=D&tipo=E&limit=100` — **capturar o 200 INTEIRO, cru** | Se `observacao` ecoar fielmente, a classe **`unconfirmed` inteira vira consulta determinística**, o gate que trava carrinho pago some e o painel manual deixa de existir. As 7 perguntas em §4.3 | ⚠️ **precisa de permissão** — ADABYTE dá 403 |
| **3** | 🔴 **`PUT /itens` num pedido apenas RESERVADO** | a sequência de **10 chamadas** de §2.4 | **A suposição sobre a qual todo o fluxo alvo se apoia.** Também responde: em que momento a reserva nativa nasce (POST? aprovação? lançamento?) e se ela acompanha a grade nova | **conta A obrigatória** |
| **4** | **`PUT /pedidos` aceita frete/desconto/endereço/itens?** | 4 PUTs num pedido descartável: `{"valorFrete":15.9}` · `{"valorDesconto":5}` · `{"enderecoEntrega":{…}}` · `{"itens":[…]}`, lendo o GET depois de cada | Decide **"pedido mutável" × "cancelar e recriar"**. Hoje a imutabilidade é `[SWAGGER]` puro num documento onde `additionalProperties`=0 — **nunca foi tentada** | qualquer |
| **5** | **`PUT /itens` × frete/desconto/parcelas** | criar com `valorFrete:15.9` + 3 parcelas → `PUT /itens` mudando a grade → `GET` | Se o `PUT /itens` zerar o frete, o desenho mutável cai mesmo que o teste 4 passe. E fecha a distribuição entre N parcelas | qualquer |

## 8.2 Contrato — roda na ADABYTE, não depende de regime de conta

| # | Teste | Request exato | O que decide |
|---|---|---|---|
| 6 | **Obrigatoriedade do POST** | `POST /pedidos {}` → catalogar `detalhes[].campo`; depois remover um a um `idContato`, `data`, `itens`, `itens[].quantidade`, `itens[].valorUnitario` | O contrato de criação **não pode ser derivado do swagger** (zero `required`) |
| 7 | **`situacao:3` no POST** | `POST /pedidos {…,"situacao":3}` → `GET /pedidos/{id}` + `GET /estoque/{id}` | Elimina uma chamada do caminho crítico (relevante a 0,5 req/s) **e fecha a janela do 409-sem-aprovação** que matou 3 pedidos pagos |
| 8 | **Matriz 10×10 de situação** | pedidos descartáveis: `0→3, 3→1, 1→2, 2→3, 3→4, 4→7, 7→5, 5→6, 6→0, 3→3`; registrar status **e corpo** | A máquina de estados não existe no contrato e **nunca houve um 400** em `/situacao`. 88/90 células desconhecidas |
| 9 | **Corpo do 409 e a janela da dedup** | POST → POST idêntico → cancelar → POST idêntico → POST no dia seguinte | Se o 409 trouxesse o id, mataria a busca por marcador no `adoptExistingOrder`. E define se cancelado libera |
| 10 | **`maxLength` de `observacoes`** | POST `/estoque` com 100 / 255 / 256 / 1000 chars, **lendo de volta pelo log (teste 2)** | Truncamento silencioso quebraria a chave de idempotência **sem erro visível** |
| 11 | **Read-after-write do marcador** | `POST /marcadores` → sondar `GET /pedidos?marcadores=` a cada **500 ms** | O "~300 ms" no código é má leitura; o real é desconhecido (só se sabe ≤112 s) |
| 12 | **Marcadores: POST × PUT × DELETE** | POST 2 tags → GET · PUT 1 tag → GET · POST tag duplicada → GET · DELETE → GET | POST acumula ou substitui? DELETE apaga **todos** (é a única via, e é destrutiva — apagaria nossa âncora). Note que o GET devolve **`cor`**, campo que não existe no POST/PUT |
| 13 | **`?nome=` e `?situacao=` nas formas** | `GET /formas-recebimento?nome=Pix&situacao=1` · `GET /formas-envio?situacao=1` | Elimina **até 5 chamadas por pedido** (R12) e talvez o segundo POST do fallback de forma de envio |
| 14 | **`limit` máximo e filtros de data** | `?limit=200/500/1000/5000`; `dataInicial` em `Y-m-d`, `d/m/Y` e com hora; pedido criado às 23:50 BRT | Sem isso não há reconciliação incremental confiável, e o timezone já mordeu |
| 15 | **Depósito no `POST /estoque`** | um POST **com** `deposito.id` e um **sem**, lendo `GET /estoque/{id}.depositos[]` depois de cada | **Toda a base empírica é mono-depósito.** A pergunta certa não é "qual depósito?" e sim "o `deposito` do POST sobrescreve o padrão?" |
| 16 | **`lancar-contas` / `estornar-contas`** | `lancar-contas` → `GET /contas-receber?idVenda={id}` → `lancar-contas` de novo → contar → `baixar` → `estornar-contas` → `situacao=2` → contar | Território virgem, mesmo risco de "lançamento fantasma" do `POST /estoque` — **mas aqui existe consulta de volta**, então dá para desenhar com verificação pós-fato |
| 17 | **Rate limit por endpoint ou global?** | rajada alternando `GET /info`, `GET /estoque/{id}`, `GET /pedidos` | Define se a fila serializada é por conta ou por (conta, endpoint) |
| 18 | **Corpo dos erros não-400** | repetir `GET /depositos` (403) gravando o corpo cru; forçar 401 com token expirado; `GET /pedidos/999999999` (404) | `[SWAGGER]` não promete corpo em nenhum erro que não seja 400. Precisamos saber o que parsear |
| 19 | **NFe: o path certo** | `POST /pedidos/{id}/gerar-nota-fiscal {"modelo":55}` → `GET /pedidos/{id}` (`idNotaFiscal` veio?) → `GET /notas/{idNota}` × `GET /notafiscal/{id}` | `[CÓDIGO tiny.go:2636/:2665]` chama um path **que não existe no swagger**; `erp/invoice.go:120-124` engole com `return nil,nil` — **o front mostra "Aguardando NFe" para sempre** |
| 20 | **Webhook: headers e retry** | sessão com a ponte do `tiny-lab` gravando headers; depois devolver 500 propositalmente 21× | Fecha o `[ABERTO]` de HMAC e verifica a regra dos "20 não-200" |
| 21 | **Duplicata na grade** | `PUT /itens` com duas linhas do mesmo `produto.id` | Aceita, mescla somando, ou 400? |
| 22 | **Kits e variações** | 1 pedido com kit + `lancar-estoque`; 1 produto com variação em `GET /estoque` e no webhook | O nosso código tem caminho de variantes que **nunca viu resposta real** |

## 8.3 Fixes que não dependem de nenhum teste

Podem entrar em paralelo à bateria, cada um numa branch a partir de `stg`:

1. ✅ **FEITO hoje** — reclassificar 4xx (notadamente 429) como `ErrProvenUndelivered` em `ReverseStockReservation`
   (commit `8e633f0`). **Falta a ação operacional sobre os dois carrinhos já travados (#1115 e #1171).**
2. **Unificar `tinyCartMarker` (`tiny.go:1964`) e `erpOrderMarker` (`order_lifecycle.go:50`)** — duas funções
   idênticas que produzem `"lc-cart-"+id`, em pacotes diferentes. **Se divergirem, a adoção de pedido por marcador
   — o único caminho de recuperação do 409 e do sweep — para de funcionar em silêncio.** Custo zero, risco alto.
3. **Dar um chamador a `ListUnresolvedERPStockMovements`** — a query existe em `db/queries/erp_stock_movement.sql:47-53`,
   é gerada pelo sqlc, o índice parcial existe desde a migration `000132`, e **não tem nenhum chamador Go**. É por
   isso que a linha `98d4d0e5` está `pending` desde 24/08 sem que ninguém vá resolvê-la.
4. **Logar o `X-Request-Id`** de toda resposta do Tiny — é o identificador que um ticket com o fornecedor exige, e
   hoje não é registrado em lugar nenhum.
5. **Apagar os comentários obsoletos** que fazem o código gastar chamadas à toa: `tiny.go:530-534` (*"o schema não
   está na documentação pública"* — está), `tiny.go:1643-1649` e `tiny.go:1733-1742` (*"não aceita filtro `nome`"* —
   aceita, e por isso paginamos até 5 páginas por pedido).
6. **Trocar o `state` cru do OAuth pela tabela `oauth_states`** (que já existe, com `code_verifier`) e adotar PKCE —
   o desenho certo já está no repo em `melhor_envio_oauth.go:44-82`, e o PKCE S256 foi validado hoje contra o Tiny.
7. **Gravar o `cpfCnpj` do `GET /info` no connect e comparar com `dados.cnpj` no `HandleTiny`** antes do
   `RecordWebhookPing` — fecha o ping forjável e o risco multi-CNPJ (LIV-85).
8. **Desligar o ticker `RunERPOrderOpsSweep`** (`main.go:770`) enquanto o Design C estiver inerte: roda a cada 5 min
   contra 308 carrinhos com `erp_order_state='none'` — **308 de 308** `[EMPÍRICO prod]`.

---

# 9. Anexo — higiene e registros do dia

## 9.1 A ferramenta

`apps/tiny-lab` (commit `1b6e51b`, branch `feat/tiny-lab-oauth-recon`) — módulo Go próprio, **só stdlib**, sem
dependências. Grava `.tiny-lab/audit.jsonl` com request, response, status, duração **e headers**; a ponte de
webhooks grava os headers também. Guard de escrita duplo (`TINY_ENV=sandbox` + allowlist de CNPJ conferida a cada
execução). **Cópia do log desta sessão em `scratchpad/audit-sessao.jsonl`.**

Nesta sessão: **só GETs e a troca de token. Nenhuma escrita** — a allowlist está vazia.

## 9.2 O que a conta ADABYTE parece hoje

`[EMPÍRICO 25/08]`

| Recurso | Estado |
|---|---|
| `GET /pedidos?limit=1` | `total: 0` — **os 27 pedidos da bateria de 11/07 não existem mais** |
| `GET /produtos?limit=1` | `total: 21` |
| `GET /contatos?limit=1` | `total: 12` — o contato `895591553` da bateria não é mais o primeiro |
| `GET /contas-receber?limit=1` | `total: 0` |
| `GET /estoque/357281337` | `saldo: 1` (era 10 em 11/07), `reservado: 0`, `disponivel: 0` |
| `/depositos`, `/estoque/{id}/logs-movimentacao` | **403** |

⚠️ **Os fixtures de 11/07 estão mortos.** Qualquer bateria nova precisa recriar contato e pedidos. Note também que
`GET /produtos` (listagem) devolve `"estoque":{"localizacao":""}` — **sem `quantidade`**, ao contrário do
`GET /produtos/{id}`.

## 9.3 Falha de teste pré-existente — registro para o AUDIT

`internal/conventions` `TestNoNewRawHttpxThrows` **falha na `stg`** por causa de `customer/vip.go` (feature Clientes
VIP). **Provado pré-existente por stash** — não é regressão desta linha de trabalho, mas precisa de dono.

## 9.4 Correções ao acervo — duas afirmações que circulam e estão erradas

1. **O "~300 ms de read-after-write do marcador"** (`tiny.go:2173-2181`, `providers/types.go:338`) **não existe nos
   dados.** É o `dur_ms` da chamada HTTP. O único fato provado é "visível em ≤112 s". Ver §3.6.
2. **`numeroPedido` string × integer e `valor` string × float NÃO são inconsistência do swagger.** `[EMPÍRICO 11/07]`
   **a API é assim mesmo**, e o swagger descreve corretamente. A conclusão prática (dois tipos distintos no cliente
   Go) continua certa; a expectativa de que "o fornecedor vai corrigir" está errada.

## 9.5 Calibração de confiança neste swagger — a regra a adotar

`[EMPÍRICO 25/08: diff programático]`

> **Schemas de RESPOSTA deste swagger são confiáveis** — verificado campo a campo em Pedidos (detalhe e listagem):
> `swagger-only: []`, `real-only: []`. **Parsers de resposta podem ser gerados dele.**
>
> **Schemas de REQUEST não são** — **45 dos 68 schemas de request distintos** não têm nenhum `required` na cadeia
> (equivalente a **65 das 89 operações com corpo**), e `additionalProperties` nunca é declarado (0 ocorrências em
> 1,1 MB). **Cada campo de escrita precisa de um teste.**
> *(O número "51 de 60" que circulava no acervo — inclusive em `08-CRITICO.md` §P3.2 — está errado; recontado
> programaticamente nesta revisão.)*

E uma qualificação obrigatória sempre que a bateria de 11/07 for citada:

> **7,4% da API (15 de 202 operações), conta mono-depósito, um contato, dois produtos sem variação e sem kit,
> nenhum pedido com endereço, frete, parcelas ou NFe.** Não deve ser citada como "o comportamento do Tiny" sem
> essa qualificação.

---

## Verificação

> Revisão **adversarial** deste documento e do `AUDIT.md`, feita em **25/08/2026** por um segundo leitor com
> acesso ao disco. Método: reabrir cada citação e tentar derrubá-la. O que sobreviveu ficou; o que caiu está
> corrigido **no lugar**, com a correção anotada onde ela mora. Nenhum código de produção foi tocado.

## O que foi conferido

| Frente | Volume | Como |
|---|---|---|
| Citações `arquivo:linha` | **~220 distintas**, nos dois documentos | `sed -n 'Np'` / `grep -n` sobre a árvore em `c7d4ced`, uma a uma |
| Citações do swagger | **~35 asserções** | `python3` sobre `/mnt/c/Users/aliss/Downloads/swagger.json` — contagens, resolução de `$ref`/`allOf`, enumeração de paths, operações e schemas |
| Contradições RECON × AUDIT | leitura integral dos dois | cruzamento de números, datas, contagens e vereditos |
| Tabelas e markdown | script | contagem de pipes por bloco, balanceamento de cercas ```` ``` ````, âncoras do índice |
| Português | script + leitura | palavra repetida, crase, espaço duplo, typos frequentes — **nenhuma ocorrência** |

**Verificado e CORRETO** (amostra do que resistiu): `1.110.748 bytes` · `127 paths / 202 operações / 348 schemas` ·
`info.version 3.1` · `servers[0]` · `additionalProperties`/`readOnly`/`writeOnly`/`deprecated`/`maxLength`/
`minLength`/`pattern`/`429`/`RateLimit`/`Retry-After`/`idempot`/`in: header` = **0 ocorrências cada** ·
`possuiReserva` **1 vez**, no `DetalheDepositoResponseModel` que serve `/depositos` **e** `/depositos/{idDeposito}`,
com a descrição literal citada · `PaginatedResultModel` = `{limit, offset, total}` e é o 200 de `logs-movimentacao` ·
`LogMovimentacaoEstoqueResponseModel` órfão, num total de **13 órfãos** · parâmetros de `logs-movimentacao` ·
`AtualizarSituacaoPedidoModelRequest` com `required:["situacao"]` e enum `[8,0,3,4,1,7,5,6,2,9]` ·
`CriarPedidoModelRequest` com **zero** `required` em toda a cadeia (24 schemas resolvidos) ·
`AtualizarPedidoModelRequest` = 7 campos (`BasePedidoModel` + `pagamento.parcelas`/`categoria` +
`pagamentosIntegrados`) · `AtualizarProdutoEstoqueModelRequest` `required:["tipo","quantidade","precoUnitario"]`,
resposta **200** · `CriarPedidoModelResponse` sem `situacao` e com `numeroPedido` **string** contra **integer** no
GET · `ItemPedidoRequestModel` sem id de linha e sem `sku` · as **duas** descrições literais do `PUT /itens` ·
os **15 filtros** de `GET /pedidos` · `ProdutoRequestModel.id` `nullable:false` · enum `fretePorConta` ·
`/formas-pagamento` e `/formas-recebimento` aceitando `nome` **e** `situacao` · **24** arrays `required` em 348
schemas · **409 não declarado** em nenhuma das 202 operações · `401/403/404/500/503` **sem `content`** em 202/202 ·
`format:float` **224×** · exemplos de data **109 / 19 / 2** · **10** operações de contas a receber ·
`valorTotalPedido` sem `description` · nenhum path contendo `notafiscal`.
No código: `tiny.go` **3.088 linhas**, `order_lifecycle.go` **702**; **28** pontos de chamada HTTP no `tiny.go`
(24 `DoRequest` + 4 `DoRequestWithRetry` nas linhas 1616/1754/1841/2184); as 6 strings de `observacoes` de §4.2;
`erp_stock_movement.sql:47-53`; `product.sql:55-60` e `:111-128`; e as migrations `000027`, `000030`, `000032`,
`000069`, `000085`, `000101`, `000106`, `000132`.

## O que foi corrigido neste documento

| # | Onde | Estava | Está |
|---|---|---|---|
| 1 | §6.1, tabela de correções | `[CÓDIGO lib/ratelimit/adaptive.go:146-149]` | **O arquivo tem 136 linhas** — a citação apontava para o vazio. Trocada por `:52-56` e `:58-63`, que são de fato o "sem header libera tudo" e o "janela rolou, zera e libera tudo" |
| 2 | §6.1, mesma linha | *"apenas 4 de ~25 chamadas"* | **4 dos 28** pontos de chamada do `tiny.go` (24 `DoRequest` + 4 `DoRequestWithRetry`) — alinhado ao número exato do AUDIT §6.1 |
| 3 | §3.1 e §9.5 | *"51 dos 60 request bodies não têm `required`"* | **45 dos 68 schemas de request distintos** (= **65 das 89 operações com corpo**). O "51 de 60" vinha de `08-CRITICO.md` §P3.2 e **não reproduz** |
| 4 | §3.6 | *"os 17 paths com 'pedido' … são exatamente os 17 da tag"* | A tag Pedidos tem **17 operações em 12 paths**. Confundia operação com path; a conclusão (não há path alternativo) sobrevive |
| 5 | §6.3 | "Exceções ao envelope" listadas como **6** | São **15** GETs com array cru na raiz; a lista completa entrou, com o padrão que a explica (marcadores e recebimentos) |
| 6 | §6.2, catálogo de erros | `meioPagamento não encontrado` atribuído ao `PUT /pedidos/{id}` | O corpo está transcrito em `[CÓDIGO tiny.go:1300-1310]`, **dentro do `CreateOrder`** → `POST /pedidos`. O comentário não registra o verbo, então ficou `[ABERTO]` se o `PUT` também recusa |
| 7 | §6.4 | `webhook_handler.go:669` | `:670` — 669 é `Payload: json.RawMessage(body)` |
| 8 | §2.2 e §2.5 | `PLANO_RESERVA_TINY.md:81-84` e `:79-86` | `:81-85` e `:79-87` — as citações cortavam justamente a frase citada (`total reservado 0,00`; `Veredito: Estratégia A está morta`) |
| 9 | §1 | *"O detector determinístico **existe**"* | *"está **declarado no contrato**"* + a lembrança explícita de que a chamada deu **403** e nem valor nem shape foram vistos. Era `[SWAGGER]` lido como comportamento |
| 10 | §2.3 | `GET /depositos` **devolve** `DetalheDepositoResponseModel` | **declara** — nenhum corpo real foi visto |
| 11 | §2.3, item 3 | *"`grep possuiReserva` = 0. Nunca foi lido."* | O campo **não era desconhecido**: `PLANO_RESERVA_TINY.md:82` já o citava em 18/08 e o **descartou** como *"só um flag do depósito"*. Não estava por ler — estava mal lido, e é isso que o item 2 desfaz |
| 12 | §3.2 | *"Substitui a grade? **SIM**"*, com `[SWAGGER]` como única fonte | Título reescrito e o lastro empírico explicitado: `[EMPÍRICO 11/07 T6-e]` `{"itens":[]}` → 204 e pedido sem itens (**num merge seria no-op**). A **remoção seletiva** virou `[ABERTO]` com a chamada que fecha |
| 13 | §7.3 C1 | **RESPONDIDO** com evidência só de swagger | **PARCIAL**, com a mesma distinção do item 12 |
| 14 | §3.3 | `AtualizarPedidoModelRequest` **aceita** sete coisas | **declara** sete coisas — o próprio bloco seguinte já dizia que declarar ≠ aceitar |
| 15 | §3.6 | Bloco `GET /pedidos?numero=1186 → 200 {…}` com id inventado, sob `[SWAGGER]` | Marcado como **forma declarada com números ilustrativos, não corpo capturado**, e `[ABERTO]` explícito: nenhuma busca por `numero` foi executada |
| 16 | §5.3 | *"qualquer reembolso parcial fica amarrado ao pedido inteiro"*, sem marca | `[ANÁLISE]` + a distinção que faltava: **ausência de operação** é conclusiva (não se chama o que não existe); **ausência de campo** não é, e é por isso que o "frete imutável" de §3.3 continua `[ABERTO]` |
| 17 | §1 e §8.3 | *"Dois carrinhos pagos travados"* | Reformulado para **duas linhas do razão não resolvidas**, com a precisão do AUDIT §5.2: o **#1171** tem `erp_finalisation_status='done'` — é vazamento de observabilidade, não de estoque. E as **finalizações falhas são três** (#1186, #1087, #1115) |
| 18 | §2.4 | Lista *"Não sabemos:"* sem marca | `[ABERTO]` explícito, apontando para a sequência de 10 chamadas da própria seção |
| 19 | §5 e §1 | `[EMPÍRICO]` para resultado de `grep` no código | `[CÓDIGO: grep]` — grep de repositório não é medição contra o ERP |
| 20 | §6.1 | `[EMPÍRICO: contagem no código]` | `[CÓDIGO: contagem dos call sites]`, com ponteiro para AUDIT §2 |
| 21 | Índice | Link `#5-contas--financeiro` | **Âncora quebrada** — o heading é "Contas / financeiro — território virgem". Corrigida |
| 22 | §4.3 | `objOrigem` `[ABERTO]` sem pista | Mantido `[ABERTO]`, com a pista encontrada nesta revisão: `NotaFiscalOrigemModelResponse.tipo` enumera `pedido_compra/venda/notafiscal/ordemservico/cobranca/devolucao`. **Nada no arquivo liga esse enum a `objOrigem`** — é hipótese para o T-L1, não resposta |

## O que NÃO foi conferido — e por quê

- **Os números de produção.** Nenhuma consulta ao Postgres de produção foi refeita nesta revisão. Tudo que está
  marcado `[EMPÍRICO prod 25/08]` é reproduzido do bloco de evidência da sessão e do AUDIT; a **aritmética interna**
  fecha (430 = 269+119+26+11+5; 646 = 416+170+35+18+7; 1.064 = 477+587; 308 = 149+91+53+15), mas os totais não
  foram relidos da fonte.
- **A bateria de 11/07 linha a linha.** Os corpos, contagens (`92` GETs, `42` POSTs, `13`×409, `75` webhooks,
  `333` requests, `52` erros) e latências vêm do `actions.jsonl`/`webhooks.jsonl` conforme relatados pelos
  relatórios anteriores. Amostrei a **coerência aritmética** (42 POSTs = 27×201 + 13×409 + 2×429; 52 erros = 30+13+3+2+2+1+1;
  52/333 = 15,6 %) — **não reabri o JSONL**. `[ABERTO]` para quem quiser fechar: reprocessar os dois arquivos.
- **`ratelimit-burst.json` e `audit-sessao.jsonl`.** Os números de rate limit vêm do bloco empírico da sessão e não
  foram recontados a partir dos arquivos crus.
- **As perguntas do brief (seções 3.2 e 9).** **O documento de brief não está no disco** — procurado em
  `/home/alisson/Desktop/livecart`, no `.claude/` do projeto e em toda a árvore de scratchpad; o
  `08-CRITICO.md` §8 registra a mesma busca infrutífera. Portanto **não foi possível cruzar item a item** quais
  perguntas do brief ficaram sem resposta. O que foi feito no lugar: varredura programática das duas peças atrás de
  **interrogações sem marca de procedência** — todas as encontradas ou já estavam dentro de um bloco `[ABERTO]`, ou
  eram URLs com query string. A única exceção real (§2.4) foi corrigida (item 18 acima).
- **Cobertura de §7 contra §§1–6.** As 7 subtabelas de cobertura foram lidas, mas não houve verificação mecânica de
  que **toda** pergunta levantada no corpo tem linha correspondente em §7.
