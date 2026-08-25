# tiny-lab

Ferramenta local de exploração da API v3 do Tiny/Olist. Existe para a **Fase 0**
da refatoração da integração: descobrir empiricamente como o ERP se comporta,
com request e response reais gravados, antes de decidir arquitetura.

Não faz parte do serviço. É um módulo Go próprio (como `apps/instagram-emulator`),
sem dependências externas — só a stdlib.

```
go build -o tiny-lab .
./tiny-lab help
```

## Por que existe um guard de conta

**A API v3 do Tiny tem host único: `https://api.tiny.com.br/public-api/v3`.**
Não existe ambiente de sandbox do lado do fornecedor. "Conta de teste" quer
dizer uma conta Tiny comum que combinamos usar para testes — e o único jeito de
saber em qual conta um token manda é perguntar ao próprio ERP.

Por isso `TINY_ENV=sandbox` sozinho não protege nada. Toda escrita
(`POST`/`PUT`/`PATCH`/`DELETE`) passa por **dois portões**:

1. `TINY_ENV=sandbox`;
2. o `cpfCnpj` devolvido por `GET /info` tem que estar em `TINY_ALLOWED_CNPJ`.

O segundo é conferido **a cada execução**, contra o ERP, antes de a requisição
sair da máquina. Não há bypass, nem flag de "eu sei o que estou fazendo".
Leitura (`GET`) nunca é bloqueada.

## Setup

```bash
cp .env.example .env      # preencha client id/secret
./tiny-lab auth login     # abre o navegador, recebe o callback, salva os tokens
./tiny-lab auth status    # mostra a conta autorizada e imprime o CNPJ a liberar
```

`auth status` imprime a linha exata de `TINY_ALLOWED_CNPJ` para colar no `.env`.
Enquanto ela estiver vazia, toda escrita fica bloqueada — é o padrão desejado.

Os tokens ficam em `.tiny-lab/tokens.json` (0600, ignorado pelo git) e são
renovados sozinhos quando faltam menos de 60s para vencer.

## Explorando a API

```bash
./tiny-lab api GET  /info
./tiny-lab api GET  /pedidos -q dataInicial=2026-08-25 -q limit=10
./tiny-lab api GET  /estoque/357281337
./tiny-lab api GET  /estoque/357281337/logs-movimentacao
./tiny-lab api POST /pedidos -d @fixtures/pedido-minimo.json --dry-run
./tiny-lab api PUT  /pedidos/358128422/itens -d '{"itens":[...]}'
```

Toda chamada — request, response, status, latência, headers de rate limit — vai
para `.tiny-lab/audit.jsonl`. É a evidência que sustenta o `RECON.md`:

```bash
./tiny-lab audit 30
jq -c 'select(.status >= 400) | {status, url, response_body}' .tiny-lab/audit.jsonl
```

## Ponte de webhooks

Recebe o que o Tiny manda e grava o payload **cru** — headers, querystring,
corpo byte a byte, horário. Sem transformação: queremos o formato real, não o
que a documentação diz que é.

```bash
./tiny-lab hooks serve            # porta 8787 (TINY_HOOKS_PORT)
./tiny-lab hooks list
./tiny-lab hooks show <id>
./tiny-lab hooks replay <id> --to http://localhost:3001/webhook/tiny --times 3
```

O handler é **catch-all**: qualquer caminho é aceito e o caminho recebido fica
gravado no evento. Numa fase de descoberta, perder uma entrega por causa de um
404 nosso seria o pior resultado possível.

`replay` reenvia o evento idêntico, N vezes, com os headers originais. É como se
testa idempotência.

### Expondo a porta para o Tiny

`cloudflared` já está instalado e não precisa de conta:

```bash
cloudflared tunnel --url http://localhost:8787
```

Ele imprime uma URL `https://<algo>.trycloudflare.com`. **Essa URL muda a cada
execução** — deixe o túnel rodando durante a sessão de testes e recadastre no
painel do Tiny quando reiniciar.

A URL a cadastrar no painel do Tiny é:

```
https://<algo>.trycloudflare.com/webhooks/tiny
```

Confira antes de cadastrar:

```bash
curl https://<algo>.trycloudflare.com/health     # deve responder "ok"
```

## Estado local

Tudo em `.tiny-lab/`, ignorado pelo git:

| arquivo | conteúdo |
|---|---|
| `tokens.json` | access + refresh token (0600) |
| `audit.jsonl` | toda chamada ao ERP e ao OAuth |
| `webhooks/*.json` | um arquivo por entrega recebida |

## O que NUNCA entra em commit

`.env`, `.tiny-lab/` e o binário — todos no `.gitignore` deste diretório.
O `audit.jsonl` não grava `Authorization`, e o log do OAuth guarda só metadado
(`expires_in`, `scope`), nunca o token.
