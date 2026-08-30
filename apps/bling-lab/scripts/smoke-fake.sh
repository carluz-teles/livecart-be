#!/usr/bin/env bash
# Valida o bling-lab INTEIRO contra um Bling de mentira: OAuth, catálogo,
# saldo e webhook assinado. Não gasta requisição da conta real, não precisa de
# credencial e não depende de túnel.
#
# É o que se roda antes de tocar na conta do lojista, e depois de toda mudança.
set -euo pipefail

cd "$(dirname "$0")/.."
RAIZ="$PWD"
ESTADO="$(mktemp -d)"
# Portas PRÓPRIAS do smoke, fora da faixa padrão do laboratório (8790/8791).
#
# O motivo é operacional: enquanto se testa contra a conta real, a ponte de
# webhooks fica de pé na 8791 com um túnel apontado para ela — e o smoke não
# pode exigir que o operador derrube a ponte para rodar os testes. Colidir ali
# fazia o smoke medir o servidor errado.
PORTA_FAKE=${PORTA_FAKE:-8899}
PORTA_CB=${PORTA_CB:-8890}
PORTA_HOOKS=${PORTA_HOOKS:-8891}

falhas=0
ok()    { printf '  \033[32m✓\033[0m %s\n' "$1"; }
falha() { printf '  \033[31m✗\033[0m %s\n' "$1"; falhas=$((falhas+1)); }
secao() { printf '\n\033[1m%s\033[0m\n' "$1"; }

limpar() {
  [ -n "${PID_FAKE:-}" ] && kill "$PID_FAKE" 2>/dev/null || true
  [ -n "${PID_HOOKS:-}" ] && kill "$PID_HOOKS" 2>/dev/null || true
  rm -rf "$ESTADO"
}
trap limpar EXIT

secao "compilando"
go build -o "$ESTADO/bling-lab" . || { falha "build do bling-lab"; exit 1; }
go build -o "$ESTADO/fake-bling" ./cmd/fake-bling || { falha "build do fake-bling"; exit 1; }
ok "bling-lab e fake-bling compilam"

secao "testes unitários"
if go test ./... > "$ESTADO/test.log" 2>&1; then ok "go test ./... passou"
else falha "go test falhou:"; sed 's/^/      /' "$ESTADO/test.log"; fi

# O laboratório aponta para o Bling falso; o estado vai para um tmpdir, então
# um .env real que exista no repositório NÃO é usado nem sobrescrito.
export BLING_CLIENT_ID=fake-client-id
export BLING_CLIENT_SECRET=fake-client-secret
export BLING_REDIRECT_URI="http://localhost:$PORTA_CB/callback"
export BLING_AUTH_URL="http://localhost:$PORTA_FAKE/Api/v3/oauth/authorize"
export BLING_TOKEN_URL="http://localhost:$PORTA_FAKE/Api/v3/oauth/token"
export BLING_REVOKE_URL="http://localhost:$PORTA_FAKE/Api/v3/oauth/revoke"
export BLING_API_BASE="http://localhost:$PORTA_FAKE/Api/v3"
export BLING_STATE_DIR="$ESTADO/estado"
export BLING_HOOKS_PORT="$PORTA_HOOKS"
export BLING_RATE_LIMIT_RPS=20

# lab() só para chamadas em PRIMEIRO PLANO. Para servidor em background o
# binário é invocado DIRETO, sem função e sem subshell: `func ... &` faz o bash
# forkar um subshell, `$!` devolve o PID DELE, e matar o subshell deixa o
# binário vivo segurando a porta. Foi assim que a segunda execução seguida
# deste script passou a falhar — e a falha era um falso NEGATIVO, o que é bom;
# o perigoso é o falso POSITIVO que isso causava antes de `exigir_porta_livre`.
lab() { "$ESTADO/bling-lab" "$@"; }

# exigir_porta_livre existe por causa de um falso-positivo real: um `hooks serve`
# zumbi de uma execução anterior segurava a porta, o servidor novo não subia, e
# os eventos iam para o processo velho — cujo log ninguém estava lendo.
exigir_porta_livre() {
  local porta=$1 pid
  pid=$(ss -lptn "sport = :$porta" 2>/dev/null | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2 || true)
  if [ -n "${pid:-}" ]; then
    falha "porta $porta já ocupada pelo pid $pid (processo zumbi de outra execução?)"
    return 1
  fi
}

secao "subindo o Bling falso"
"$ESTADO/fake-bling" --porta "$PORTA_FAKE" --redirect "$BLING_REDIRECT_URI" > "$ESTADO/fake.log" 2>&1 &
PID_FAKE=$!
for _ in $(seq 1 30); do
  curl -sf --max-time 1 "http://localhost:$PORTA_FAKE/Api/v3/depositos" -o /dev/null && break
  # 401 também significa "de pé" — o endpoint exige token.
  curl -s --max-time 1 -o /dev/null -w '%{http_code}' "http://localhost:$PORTA_FAKE/Api/v3/depositos" | grep -q 401 && break
  sleep 0.2
done
ok "fake-bling de pé na porta $PORTA_FAKE"

secao "OAuth — authorization_code ponta a ponta"
"$ESTADO/bling-lab" auth login --no-browser > "$ESTADO/login.log" 2>&1 &
PID_LOGIN=$!
for _ in $(seq 1 40); do grep -q 'oauth/authorize' "$ESTADO/login.log" 2>/dev/null && break; sleep 0.1; done
URL=$(grep -oE "http://localhost:$PORTA_FAKE[^ ]*" "$ESTADO/login.log" | head -1)
[ -n "$URL" ] && ok "URL de autorização montada" || falha "não achei a URL de autorização"
grep -q 'redirect_uri' <<<"$URL" && falha "mandou redirect_uri (o Bling ignora)" || ok "não manda redirect_uri nem scope"
curl -sSL --max-time 15 "$URL" -o /dev/null
wait $PID_LOGIN 2>/dev/null || true

grep -q 'token salvo' "$ESTADO/login.log" && ok "token obtido e salvo" || { falha "login falhou:"; sed 's/^/      /' "$ESTADO/login.log"; }
grep -q '436c56a5679921f5f13a3d6433561773' "$ESTADO/login.log" && ok "identidade da conta lida" || falha "não leu a identidade"
grep -q 'credencial veio no BODY' "$ESTADO/fake.log" && falha "credencial foi no body — o Bling recusa" || ok "credencial foi no header Basic"

secao "catálogo"
lab produtos > "$ESTADO/prod.log" 2>&1
grep -q 'VST-001' "$ESTADO/prod.log" && grep -q 'BLS-002' "$ESTADO/prod.log" \
  && ok "criterio=5 traz os 2 produtos" || falha "listagem incompleta"
lab produtos --criterio 1 > "$ESTADO/prod1.log" 2>&1
grep -q 'BLS-002' "$ESTADO/prod1.log" \
  && falha "criterio=1 devia esconder produto (armadilha do default)" \
  || ok "criterio=1 (default do Bling) esconde produto — armadilha reproduzida"

secao "estoque em lote"
lab saldos 16234567890 16234567891 --filtro 1 > "$ESTADO/saldo.log" 2>&1
grep -q 'NÃO vieram na resposta' "$ESTADO/saldo.log" \
  && ok "produto esgotado some com filtro=1 — e o lab AVISA" \
  || falha "o lab não avisou sobre o produto ausente"

secao "probe — a rodada de medição"
lab probe > "$ESTADO/probe.log" 2>&1
grep -q 'NENHUM. Confirmado' "$ESTADO/probe.log" && ok "ausência de header de cota detectada" || falha "não detectou a ausência de header"
grep -q 'DIVERGE' "$ESTADO/probe.log" && ok "divergência do saldoVirtualTotal flagrada" || falha "não flagrou a divergência do saldo"
grep -q 'dívida D5 é REAL' "$ESTADO/probe.log" && ok "depósito com desconsiderarSaldo detectado" || falha "não detectou o depósito excluído"

secao "webhooks"
exigir_porta_livre "$PORTA_HOOKS" || true
"$ESTADO/bling-lab" hooks serve > "$ESTADO/hooks.log" 2>&1 &
PID_HOOKS=$!
for _ in $(seq 1 30); do curl -sf --max-time 1 "http://localhost:$PORTA_HOOKS/health" -o /dev/null && break; sleep 0.2; done
grep -q 'modo OBSERVAÇÃO' "$ESTADO/hooks.log" \
  && ok "servidor subiu em modo observação" \
  || { falha "o servidor de webhooks não subiu (porta ocupada?)"; sed 's/^/      /' "$ESTADO/hooks.log"; }

DEST="http://localhost:$PORTA_HOOKS/webhooks/bling"
curl -sS -X POST "http://localhost:$PORTA_FAKE/disparar-webhook?destino=$DEST&evento=stock.updated" -o "$ESTADO/w1.json"
curl -sS -X POST "http://localhost:$PORTA_FAKE/disparar-webhook?destino=$DEST&evento=product.updated&assinatura=errada" -o "$ESTADO/w2.json"
sleep 1
grep -q '"status":200' "$ESTADO/w1.json" && ok "evento válido respondido com 200 (o Bling exige 2xx em 5s)" || falha "não respondeu 200"
grep -q 'assinatura valid'    "$ESTADO/hooks.log" && ok "HMAC-SHA256 válido reconhecido"   || falha "não validou a assinatura boa"
grep -q 'assinatura mismatch' "$ESTADO/hooks.log" && ok "HMAC adulterado reconhecido"      || falha "não flagrou a assinatura ruim"
kill "$PID_HOOKS" 2>/dev/null || true; wait "$PID_HOOKS" 2>/dev/null || true; PID_HOOKS=""

# O companyId do webhook tem de casar com o id de /empresas/me/dados-basicos —
# é a premissa da chave de cota E do roteamento por URL única.
if grep -q 'empresa=436c56a5679921f5f13a3d6433561773' "$ESTADO/hooks.log"; then
  ok "companyId do webhook == id de /empresas/me/dados-basicos"
else
  falha "o companyId do webhook não casou com a identidade da conta"
fi

secao "modo estrito"
# Porta PRÓPRIA: reusar a anterior corre o risco de o servidor velho ainda
# segurar o socket, o novo não subir, e o teste medir o servidor errado —
# passando por engano. Foi exatamente o que aconteceu na primeira versão.
PORTA_ESTRITO=$((PORTA_HOOKS + 1))
exigir_porta_livre "$PORTA_ESTRITO" || true
BLING_HOOKS_PORT=$PORTA_ESTRITO "$ESTADO/bling-lab" hooks serve --estrito > "$ESTADO/hooks2.log" 2>&1 &
PID_HOOKS=$!
for _ in $(seq 1 30); do curl -sf --max-time 1 "http://localhost:$PORTA_ESTRITO/health" -o /dev/null && break; sleep 0.2; done
grep -q 'modo ESTRITO' "$ESTADO/hooks2.log" && ok "servidor subiu em modo estrito" || falha "o servidor estrito não subiu"
curl -sS -X POST "http://localhost:$PORTA_FAKE/disparar-webhook?destino=http://localhost:$PORTA_ESTRITO/webhooks/bling&assinatura=errada" -o "$ESTADO/w3.json"
sleep 0.5
grep -q '"status":401' "$ESTADO/w3.json" && ok "modo estrito recusa assinatura inválida com 401" || falha "modo estrito não recusou"
kill "$PID_HOOKS" 2>/dev/null || true; wait "$PID_HOOKS" 2>/dev/null || true; PID_HOOKS=""

secao "renovação"
lab auth refresh > "$ESTADO/refresh.log" 2>&1
grep -q 'ROTACIONOU' "$ESTADO/refresh.log" && ok "rotação do refresh token medida e reportada" || falha "não mediu a rotação"

secao "guard de escrita"
if lab api /produtos --headers > /dev/null 2>&1; then ok "leitura liberada"; else falha "leitura devia funcionar"; fi

printf '\n'
if [ "$falhas" -eq 0 ]; then
  printf '\033[32m═══ tudo passou ═══\033[0m\n'
else
  printf '\033[31m═══ %d falha(s) ═══\033[0m\n' "$falhas"
  exit 1
fi
