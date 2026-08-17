package customer

// Portão de entrada da busca de arrobas.
//
// A regra que carrega a feature: SEM termo a busca não roda. O pedido do
// lojista foi explícito — não listar a plateia inteira, só o resultado de uma
// busca. Um termo opcional transformaria a tela numa lista de centenas de
// arrobas, onde bloquear o perfil certo vira caça ao erro.

import (
	"errors"
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

func TestSearchHandlesRequestValidate(t *testing.T) {
	casos := []struct {
		nome     string
		req      SearchHandlesRequest
		quebra   bool
		chaveErr string
	}{
		{
			nome: "trecho válido",
			req:  SearchHandlesRequest{Term: "cantodaart"},
		},
		{
			nome: "dois caracteres é o piso e passa",
			req:  SearchHandlesRequest{Term: "ca"},
		},
		{
			nome:     "termo vazio é recusado — é o caso que listaria a plateia inteira",
			req:      SearchHandlesRequest{},
			quebra:   true,
			chaveErr: "search",
		},
		{
			nome:     "um caractere não discrimina",
			req:      SearchHandlesRequest{Term: "a"},
			quebra:   true,
			chaveErr: "search",
		},
		{
			nome:     "termo absurdamente longo não é arroba",
			req:      SearchHandlesRequest{Term: "abcdefghijklmnopqrstuvwxyz0123456789"},
			quebra:   true,
			chaveErr: "search",
		},
	}

	for _, tt := range casos {
		t.Run(tt.nome, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.quebra && err == nil {
				t.Fatal("esperava recusa, passou")
			}
			if !tt.quebra {
				if err != nil {
					t.Fatalf("esperava aceitar, recusou: %v", err)
				}
				return
			}

			var verrs validation.Errors
			if !errors.As(err, &verrs) {
				t.Fatalf("erro não é validation.Errors (%T) — o ErrorHandler não "+
					"renderizaria 422 com o campo", err)
			}
			if _, ok := verrs[tt.chaveErr]; !ok {
				t.Errorf("erro não aponta a chave %q: %v", tt.chaveErr, verrs)
			}
		})
	}
}

// Um RuneLength cru mediria o que veio na URL, não o arroba. "@a" tem duas
// runas e passaria; "@" sozinho tem uma, mas o caso perigoso é o que ele vira
// depois de normalizar: string VAZIA, que como substring casa a plateia inteira.
// Por isso o piso mede o termo já normalizado — e mede dentro do ozzo, para a
// recusa sair como validation.Errors com a chave do campo.
func TestSearchHandlesValidateMedeOTermoNormalizado(t *testing.T) {
	for _, termo := range []string{"@", "@a", " @b "} {
		err := (SearchHandlesRequest{Term: termo}).Validate()
		if err == nil {
			t.Errorf("termo %q foi aceito — sobra menos de 2 caracteres de arroba "+
				"depois de normalizar, e a busca casaria quase toda a plateia", termo)
			continue
		}
		var verrs validation.Errors
		if !errors.As(err, &verrs) {
			t.Errorf("termo %q recusado fora de validation.Errors (%T) — o front "+
				"não destacaria o input", termo, err)
			continue
		}
		if _, ok := verrs["search"]; !ok {
			t.Errorf("recusa de %q não aponta a chave \"search\": %v", termo, verrs)
		}
	}
}

// O caminho normal: o @ colado pelo lojista é removido e a caixa alta vira
// minúscula, para casar com o handle gravado no bloqueio.
func TestSearchHandlesToInputNormalizaOArroba(t *testing.T) {
	const loja = "11111111-1111-1111-1111-111111111111"

	input, err := (SearchHandlesRequest{Term: "@CantoDaArt"}).ToInput(loja)
	if err != nil {
		t.Fatalf("arroba com @ e maiúscula deveria ser aceito: %v", err)
	}
	if input.Term != "cantodaart" {
		t.Errorf("termo normalizado = %q, esperava %q", input.Term, "cantodaart")
	}
	if input.Limit != searchHandlesMaxResults {
		t.Errorf("limite = %d, esperava o teto %d", input.Limit, searchHandlesMaxResults)
	}
}

func TestSearchHandlesToInputRecusaLojaInvalida(t *testing.T) {
	if _, err := (SearchHandlesRequest{Term: "fulano"}).ToInput("nao-e-uuid"); err == nil {
		t.Error("store id inválido foi aceito")
	}
}
