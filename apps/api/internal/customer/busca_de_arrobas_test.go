package customer

// Busca de arrobas para a lista de perfis bloqueados.
//
// O caso que originou a feature: a lojista tem contas secundárias no Instagram
// que ela usa para INSTRUIR a audiência ("manda 1042 2" para ensinar o formato).
// O LiveCart lê aquilo como intenção de compra e abre pedido no nome dela mesma.
// Para bloquear essas contas ela precisa encontrá-las — e elas podem nunca ter
// virado `customers`, porque aquela tabela só ganha linha depois de um pedido.
// A plateia de verdade está em live_comments.
//
// Invariantes travadas:
//   B1 acha por TRECHO do arroba (o lojista não lembra o handle inteiro)
//   B2 modo exato não devolve os parecidos — é o desempate de handles vizinhos
//   B3 agrupa por arroba com contagem de mensagens (o sinal que identifica a
//      conta de instrução: fala muito e gera pedido)
//   B4 marca quem já está bloqueado, para a tela não oferecer bloquear de novo
//   B5 escopo de loja: arroba de outra loja nunca aparece
//   B6 curinga digitado é texto literal, não curinga

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

// ─── seed ───────────────────────────────────────────────────────────────────

type buscaSeed struct {
	storeID string
	eventID string
}

func seedLojaComLive(t *testing.T, tag string) buscaSeed {
	t.Helper()
	ctx := context.Background()
	var s buscaSeed

	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ($1, $2) RETURNING id::text`,
		tag, tag+"-slug",
	).Scan(&s.storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := custTestPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1, 'ended', 'Live', now()) RETURNING id::text`,
		s.storeID,
	).Scan(&s.eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return s
}

// comentar grava uma mensagem daquele arroba na live.
func comentar(t *testing.T, eventID, handle, texto, result string) {
	t.Helper()
	if _, err := custTestPool.Exec(context.Background(),
		`INSERT INTO live_comments (event_id, platform, platform_comment_id, platform_user_id,
		   platform_handle, text, result)
		 VALUES ($1, 'instagram', 'c-'||gen_random_uuid()::text, 'u-1', $2, $3, $4)`,
		eventID, handle, texto, result,
	); err != nil {
		t.Fatalf("comentar: %v", err)
	}
}

func buscar(t *testing.T, storeID, termo string, exato bool) []sqlc.SearchStoreHandlesRow {
	t.Helper()
	var sid pgtype.UUID
	if err := sid.Scan(storeID); err != nil {
		t.Fatalf("store uuid: %v", err)
	}
	rows, qerr := custTestQueries.SearchStoreHandles(context.Background(), sqlc.SearchStoreHandlesParams{
		StoreID:    sid,
		Term:       termo,
		ExactMatch: exato,
		RowLimit:   50,
	})
	if qerr != nil {
		t.Fatalf("SearchStoreHandles(%q, exato=%v): %v", termo, exato, qerr)
	}
	return rows
}

func handlesDe(rows []sqlc.SearchStoreHandlesRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Handle
	}
	return out
}

func contem(lista []string, alvo string) bool {
	for _, v := range lista {
		if v == alvo {
			return true
		}
	}
	return false
}

// ─── B1 + B2 + B3 ───────────────────────────────────────────────────────────

func TestBuscaDeArrobas_PorTrechoEExato(t *testing.T) {
	requireCustDB(t)
	s := seedLojaComLive(t, "busca1")

	// A conta de instrução da loja: fala muito e gera pedido indevido.
	comentar(t, s.eventID, "cantodaart.oficial", "manda 1042 2", "added_to_cart")
	comentar(t, s.eventID, "cantodaart.oficial", "1042 1", "added_to_cart")
	comentar(t, s.eventID, "cantodaart.oficial", "gente é assim que pede", "no_intent")
	// Uma compradora de verdade, com arroba parecido.
	comentar(t, s.eventID, "cantodaart_fan", "quero o 1042", "added_to_cart")
	// Alguém sem relação com o termo.
	comentar(t, s.eventID, "mariazinha", "amei", "no_intent")

	// ─── B1: trecho encontra as duas parecidas ───────────────────────────────
	porTrecho := handlesDe(buscar(t, s.storeID, "cantodaart", false))
	if !contem(porTrecho, "cantodaart.oficial") || !contem(porTrecho, "cantodaart_fan") {
		t.Errorf("B1: busca por trecho devolveu %v; esperava as duas parecidas — o "+
			"lojista não lembra o handle inteiro", porTrecho)
	}
	if contem(porTrecho, "mariazinha") {
		t.Errorf("B1: busca por trecho trouxe arroba sem relação: %v", porTrecho)
	}

	// ─── B2: exato desempata ────────────────────────────────────────────────
	exato := handlesDe(buscar(t, s.storeID, "cantodaart.oficial", true))
	if len(exato) != 1 || exato[0] != "cantodaart.oficial" {
		t.Errorf("B2: modo exato devolveu %v; deveria devolver só o arroba pedido — "+
			"é ele que evita bloquear a compradora de nome parecido", exato)
	}

	// ─── B3: contagem identifica a conta de instrução ───────────────────────
	rows := buscar(t, s.storeID, "cantodaart.oficial", true)
	if rows[0].MessageCount != 3 {
		t.Errorf("B3: message_count=%d, esperava 3 (as três mensagens dela)", rows[0].MessageCount)
	}
	if rows[0].CartMessageCount != 2 {
		t.Errorf("B3: cart_message_count=%d, esperava 2 — é o número que mostra que "+
			"a conta está gerando pedido indevido", rows[0].CartMessageCount)
	}
	if !rows[0].LastSeenAt.Valid {
		t.Error("B3: last_seen_at veio nulo; a tela ordena e mostra por ele")
	}
}

// ─── B4 ─────────────────────────────────────────────────────────────────────

func TestBuscaDeArrobas_MarcaQuemJaEstaBloqueado(t *testing.T) {
	requireCustDB(t)
	ctx := context.Background()
	s := seedLojaComLive(t, "busca2")

	comentar(t, s.eventID, "conta.bloqueada", "1042 1", "added_to_cart")
	comentar(t, s.eventID, "conta.livre", "1042 1", "added_to_cart")

	var sid pgtype.UUID
	if err := sid.Scan(s.storeID); err != nil {
		t.Fatalf("store uuid: %v", err)
	}
	if _, err := custTestQueries.BlockHandle(ctx, sqlc.BlockHandleParams{
		StoreID:        sid,
		PlatformHandle: "conta.bloqueada",
	}); err != nil {
		t.Fatalf("BlockHandle: %v", err)
	}

	porStatus := map[string]bool{}
	for _, r := range buscar(t, s.storeID, "conta", false) {
		porStatus[r.Handle] = r.Blocked
	}

	if !porStatus["conta.bloqueada"] {
		t.Error("B4: arroba bloqueado não veio marcado — a tela ofereceria bloquear " +
			"de novo quem já está bloqueado")
	}
	if porStatus["conta.livre"] {
		t.Error("B4: arroba livre veio marcado como bloqueado")
	}

	// Desbloquear tem de refletir na busca: o bloqueio inativo não conta.
	if _, err := custTestQueries.UnblockHandle(ctx, sqlc.UnblockHandleParams{
		StoreID:        sid,
		PlatformHandle: "conta.bloqueada",
	}); err != nil {
		t.Fatalf("UnblockHandle: %v", err)
	}
	for _, r := range buscar(t, s.storeID, "conta.bloqueada", true) {
		if r.Blocked {
			t.Error("B4: arroba desbloqueado continua aparecendo como bloqueado")
		}
	}
}

// ─── B5 ─────────────────────────────────────────────────────────────────────

func TestBuscaDeArrobas_NaoAtravessaLojas(t *testing.T) {
	requireCustDB(t)
	minha := seedLojaComLive(t, "busca3a")
	outra := seedLojaComLive(t, "busca3b")

	comentar(t, minha.eventID, "compartilhado", "1042 1", "added_to_cart")
	comentar(t, outra.eventID, "sodaoutraloja", "1042 1", "added_to_cart")

	achados := handlesDe(buscar(t, minha.storeID, "loja", false))
	if contem(achados, "sodaoutraloja") {
		t.Errorf("B5: busca vazou arroba de outra loja: %v", achados)
	}

	// E o contrário: buscando por um trecho que só existe na outra loja, nada vem.
	if rows := buscar(t, minha.storeID, "sodaoutraloja", true); len(rows) != 0 {
		t.Errorf("B5: modo exato achou %d linha(s) de arroba de outra loja", len(rows))
	}
}

// ─── B6 ─────────────────────────────────────────────────────────────────────

// O termo é digitado pelo lojista. Num LIKE, `%` e `_` seriam curingas: buscar
// "a_b" casaria "aXb" e ele bloquearia o perfil errado. A busca compara por
// substring literal, então curinga é só caractere.
func TestBuscaDeArrobas_CuringaEhTextoLiteral(t *testing.T) {
	requireCustDB(t)
	s := seedLojaComLive(t, "busca4")

	comentar(t, s.eventID, "aXb.loja", "oi", "no_intent")
	comentar(t, s.eventID, "a_b.loja", "oi", "no_intent")

	achados := handlesDe(buscar(t, s.storeID, "a_b", false))
	if contem(achados, "aXb.loja") {
		t.Errorf("B6: `_` foi tratado como curinga e casou aXb.loja: %v", achados)
	}
	if !contem(achados, "a_b.loja") {
		t.Errorf("B6: busca literal por `a_b` não achou a_b.loja: %v", achados)
	}

	if rows := buscar(t, s.storeID, "%", false); len(rows) != 0 {
		t.Errorf("B6: `%%` funcionou como curinga e devolveu %d arroba(s) — "+
			"listaria a plateia inteira", len(rows))
	}
}
