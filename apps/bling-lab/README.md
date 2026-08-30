# bling-lab

Ferramenta local de exploração da API v3 do Bling. Espelha o `apps/tiny-lab`, com
três diferenças que vêm da API e não de gosto:

| | Tiny | Bling |
|---|---|---|
| Credencial no token endpoint | no body | **só no header Basic** (body é recusado) |
| Header de cota | às vezes vem | **nunca vem** (medido) → freio preditivo |
| Assinatura de webhook | não existe | **HMAC-SHA256** em `X-Bling-Signature-256` |

**O Bling não tem sandbox.** Todo experimento roda contra a conta real de um
lojista. Por isso o laboratório é **só leitura** por padrão, e a escrita exige
duas chaves independentes (`BLING_ALLOW_WRITE=true` **e** o id da conta em
`BLING_ALLOWED_COMPANY_ID`, conferido contra `GET /empresas/me/dados-basicos`
antes de qualquer verbo de escrita sair da máquina).

---

## Validar tudo sem credencial nenhuma

Existe um Bling de mentira (`cmd/fake-bling`) que imita o que foi **medido** da
API real, incluindo os defeitos: exige Basic, recusa credencial no body, o code
vale 1 minuto e é de uso único, nenhuma resposta traz header de cota, e
`GET /produtos` discorda de `GET /estoques/saldos` no `saldoVirtualTotal`.

```bash
./scripts/smoke-fake.sh
```

Isso compila, roda os testes unitários, sobe o Bling falso, faz o login OAuth
inteiro, lista produtos, lê saldo em lote, roda o `probe`, dispara webhooks
assinados e adulterados, testa o modo estrito e mede a rotação do refresh token.
Roda em segundos e não gasta uma requisição da conta do lojista.

---

## Contra a conta real

### 1. Credenciais

```bash
cp .env.example .env    # e preencha CLIENT_ID e CLIENT_SECRET do aplicativo
```

### 2. O redirect

O Bling **ignora** o `redirect_uri` da requisição e usa sempre o do cadastro do
aplicativo. Então o valor em `.env` tem de ser **idêntico** ao cadastrado.

Tente primeiro `http://localhost:8790/callback`. Se o Bling aceitar localhost,
o login não precisa de túnel nenhum.

### 3. O túnel (obrigatório para webhooks)

Webhooks precisam de URL pública, e a configuração é feita **na UI do
aplicativo** — o Bling não tem API para registrar webhook.

```bash
# terminal 1 — a ponte
go build -o bling-lab . && ./bling-lab hooks serve

# terminal 2 — o túnel
cloudflared tunnel --url http://localhost:8791
```

O `cloudflared` imprime uma URL `https://<aleatório>.trycloudflare.com`. Cadastre
`https://<aleatório>.trycloudflare.com/webhooks/bling` na aba **Webhooks** do
aplicativo, marque os eventos de `product` e `stock`, e salve.

⚠ A URL do quick tunnel **muda a cada execução**. Enquanto for laboratório, isso
significa reeditar a URL na UI a cada sessão. Para uma URL estável, use um túnel
nomeado do Cloudflare apontando para um subdomínio de `livecart.com.br` — vale a
pena a partir do momento em que houver mais de uma sessão de teste por semana.

Alternativa que já existe no repositório: o `docker-compose.yml` sobe um
`localtunnel` com subdomínio fixo (`livecart-api.loca.lt`). O subdomínio é
estável, mas o loca.lt intercala uma página de aviso que pode atrapalhar o
redirect do OAuth no navegador. Para webhook (POST servidor-a-servidor) costuma
passar.

### 4. Autorizar e medir

```bash
./bling-lab auth login      # abre o navegador; o code vale 1 MINUTO
./bling-lab probe           # a rodada de medição — 5 requisições
```

O `probe` responde, contra a conta real, as perguntas que nenhum spec responde:
qual é o `id` da empresa e seu formato; se existe algum header de cota; quantos
depósitos a conta tem e se algum é marcado para desconsiderar saldo; e se
`GET /produtos` e `GET /estoques/saldos` divergem no `saldoVirtualTotal`.

---

## Comandos

```
auth login | status | refresh | revoke --sim
empresa
produtos [--criterio N] [--tipo T] [--limite N] [--pagina N] [--json]
produto <id>
variacoes <idPai>
saldos <id...> [--filtro 0|1|2] [--json]
depositos
api <caminho> [k=v ...] [--headers]
probe
hooks serve [--forward URL] [--estrito]
hooks list | show <id> | replay <id> <url> [--vezes N]
audit [--n N]
config
```

Tudo o que sai daqui é gravado em `.bling-lab/audit.jsonl`, com os headers de
resposta **inteiros** (menos `Set-Cookie`). Isso é deliberado: provar a
*ausência* de header de cota contra a conta real só vale se a coleta for
completa.

---

## Armadilhas que o laboratório torna visíveis

**`criterio` na listagem de produtos.** O default do Bling é `1` (últimos
incluídos) e **esconde produto**. O laboratório manda `5` (todos) por padrão, e
`--criterio 1` reproduz a armadilha.

**`filtroSaldoEstoque` no saldo.** O default é `1` (só positivo), então um
produto **esgotado simplesmente não vem na resposta**. Quem tratar ausência como
"não sei" mantém o saldo velho para sempre e vende o que não tem. O comando
`saldos` compara o que foi pedido com o que voltou e avisa quais faltaram.

**`saldoVirtualTotal` descrito ao contrário.** Em `GET /estoques/saldos` o spec
diz "desconsiderando produtos reservados"; em `GET /produtos`, para o campo de
mesmo nome, diz "considerando a reserva de estoque". Os dois números **não são
intercambiáveis**, e qual deles alimenta o portão local é decisão de arquitetura.
O `probe` põe os dois lado a lado.

**O `code` de uso único.** A doc do Bling avisa que reusar um authorization code
ainda válido faz o usuário ter "o seu acesso revogado por medidas de segurança".
Por isso a troca do code **não tem retry** — uma segunda tentativa não é uma
chance a mais, é o risco de desconectar o lojista.

**`revoke_action` / `revoke_target`.** Existem na API e revogam **todos** os
tokens de um usuário ou empresa. Num aplicativo compartilhado isso derruba lojas
que não são esta, então o `Revoke` deste laboratório **não os aceita** — a
proibição é de código, com teste, não de disciplina.
