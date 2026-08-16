package store

// Prova de ida E VOLTA da parcela mínima.
//
// O primeiro corte gravava e não lia: a coluna era escrita pelo UPDATE, mas a
// resposta do GET é montada a partir do objeto de domínio CartSettings, e o
// campo não existia lá. O lojista salvava R$ 20, dava F5 e via zero de novo —
// sem erro nenhum, porque a escrita funcionava.
//
// É a classe de bug que um teste de escrita sozinho não pega. Este vai até o
// fim: grava pelo repositório e relê pelo mesmo caminho que serve o painel.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func seedStoreParaParcelamento(t *testing.T) string {
	t.Helper()
	var id string
	slug := fmt.Sprintf("parcela-%d", time.Now().UnixNano())
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO stores (name, slug) VALUES ('Loja Parcelamento', $1) RETURNING id::text`,
		slug,
	).Scan(&id); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return id
}

func TestParcelaMinimaSobreviveAoRefresh(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	storeID := seedStoreParaParcelamento(t)

	base := UpdateCartSettingsParams{
		ID:                        storeID,
		Enabled:                   true,
		ExpirationMinutes:         30,
		ReserveStock:              true,
		MaxQuantityPerItem:        5,
		AllowEdit:                 true,
		CheckoutSendMethods:       []string{"public_link"},
		ExpirationReminderMinutes: 15,
	}

	t.Run("valor gravado volta na leitura", func(t *testing.T) {
		p := base
		p.MinInstallmentCents = 2000 // R$ 20
		if _, err := testRepo.UpdateCartSettings(ctx, p); err != nil {
			t.Fatalf("gravando: %v", err)
		}

		lido, err := testRepo.GetByID(ctx, storeID)
		if err != nil {
			t.Fatalf("lendo: %v", err)
		}
		if got := lido.CartSettings().MinInstallmentCents(); got != 2000 {
			t.Errorf("depois do refresh a loja diz %d, e o lojista salvou 2000 — "+
				"a escrita funciona, a leitura é que perdia o campo", got)
		}
	})

	t.Run("chega ate a resposta que o painel le", func(t *testing.T) {
		// O bug morava AQUI, e não na leitura do banco: a resposta do GET é
		// montada por NewStoreResponse a partir do domínio, e o campo não estava
		// no mapeamento. Um teste que parasse no objeto de domínio passaria com o
		// bug em pé — foi o que aconteceu na primeira tentativa deste teste.
		p := base
		p.MinInstallmentCents = 2000
		if _, err := testRepo.UpdateCartSettings(ctx, p); err != nil {
			t.Fatalf("gravando: %v", err)
		}
		lido, err := testRepo.GetByID(ctx, storeID)
		if err != nil {
			t.Fatalf("lendo: %v", err)
		}
		resp := NewStoreResponse(lido)
		if got := resp.CartSettings.MinInstallmentCents; got != 2000 {
			t.Errorf("a resposta do painel traz %d — o valor está no banco e no "+
				"domínio, e some no mapeamento do DTO. É exatamente o que o lojista "+
				"vê como 'salvei e voltou pra zero'", got)
		}
	})

	t.Run("zero volta como zero", func(t *testing.T) {
		// Zero é o valor válido para "sem mínimo". Se ele fosse tratado como
		// ausente em algum ponto da cadeia, desligar o mínimo seria impossível.
		p := base
		p.MinInstallmentCents = 0
		if _, err := testRepo.UpdateCartSettings(ctx, p); err != nil {
			t.Fatalf("gravando zero: %v", err)
		}
		lido, err := testRepo.GetByID(ctx, storeID)
		if err != nil {
			t.Fatalf("lendo: %v", err)
		}
		if got := lido.CartSettings().MinInstallmentCents(); got != 0 {
			t.Errorf("desligar o mínimo não pegou: %d", got)
		}
	})

	t.Run("mexer no minimo nao apaga os vizinhos", func(t *testing.T) {
		// O UPDATE escreve a linha inteira. Um parâmetro fora de ordem gravaria
		// o mínimo por cima de outra coluna — e o sintoma apareceria longe daqui.
		p := base
		p.MinInstallmentCents = 3500
		p.MaxQuantityPerItem = 7
		p.ExpirationMinutes = 45
		if _, err := testRepo.UpdateCartSettings(ctx, p); err != nil {
			t.Fatalf("gravando: %v", err)
		}
		lido, err := testRepo.GetByID(ctx, storeID)
		if err != nil {
			t.Fatalf("lendo: %v", err)
		}
		cs := lido.CartSettings()
		if cs.MinInstallmentCents() != 3500 {
			t.Errorf("mínimo = %d, quero 3500", cs.MinInstallmentCents())
		}
		if cs.MaxQuantityPerItem() != 7 {
			t.Errorf("máximo por item = %d, quero 7 — o parâmetro do mínimo caiu na "+
				"coluna errada", cs.MaxQuantityPerItem())
		}
		if cs.ExpirationMinutes() != 45 {
			t.Errorf("expiração = %d, quero 45", cs.ExpirationMinutes())
		}
	})
}
