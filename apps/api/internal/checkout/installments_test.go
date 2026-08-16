package checkout

// A parcela mínima existe porque o LiveCart oferecia 1 a 12 fixo, dividindo o
// total por N. Numa venda de R$ 60 isso vira "12× de R$ 5,00" — e o lojista paga
// a MDR de doze parcelas sobre sessenta reais sem ter como impedir.

import "testing"

func TestMaxInstallmentsFor(t *testing.T) {
	casos := []struct {
		nome    string
		total   int64
		minimo  int
		querMax int
	}{
		{
			// O comportamento de hoje, e o padrão da coluna: nenhuma loja muda
			// de comportamento ao subir a migration.
			nome: "sem mínimo configurado mantém o teto", total: 6000, minimo: 0, querMax: 12,
		},
		{
			// O caso que motivou tudo: R$ 60 com mínimo de R$ 20 oferece 3, não 12.
			nome: "mínimo encurta a lista", total: 6000, minimo: 2000, querMax: 3,
		},
		{
			nome: "divisão exata usa todas as parcelas", total: 6000, minimo: 500, querMax: 12,
		},
		{
			// 6000/700 = 8.57 → 8 parcelas de R$ 8,57. A nona ficaria abaixo do
			// mínimo, então não é oferecida.
			nome: "divisão inexata arredonda para baixo", total: 6000, minimo: 700, querMax: 8,
		},
		{
			// À vista TEM de existir mesmo abaixo do mínimo. Recusar aqui
			// deixaria uma venda de R$ 4,90 sem forma de pagar numa loja que
			// configurou mínimo de R$ 20.
			nome: "total menor que o mínimo ainda permite à vista", total: 490, minimo: 2000, querMax: 1,
		},
		{
			nome: "total igual ao mínimo permite exatamente uma", total: 2000, minimo: 2000, querMax: 1,
		},
		{
			// O mínimo só ENCURTA. Um mínimo de um centavo não pode virar 6000
			// opções de parcelamento na tela.
			nome: "mínimo minúsculo não passa do teto", total: 6000, minimo: 1, querMax: 12,
		},
		{
			nome: "total zero cai para à vista", total: 0, minimo: 2000, querMax: 1,
		},
		{
			// Defensivo: total negativo não existe no fluxo, mas devolver 12 aqui
			// ofereceria parcelamento sobre um valor inválido.
			nome: "total negativo cai para à vista", total: -100, minimo: 2000, querMax: 1,
		},
		{
			nome: "mínimo negativo é tratado como ausente", total: 6000, minimo: -5, querMax: 12,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			got := MaxInstallmentsFor(c.total, c.minimo)
			if got != c.querMax {
				t.Errorf("MaxInstallmentsFor(%d, %d) = %d, quero %d",
					c.total, c.minimo, got, c.querMax)
			}
		})
	}
}

// Toda parcela oferecida tem de respeitar o mínimo. É a propriedade que a função
// existe para garantir, e um off-by-one aqui devolve exatamente o problema que
// ela veio resolver.
func TestNenhumaParcelaOferecidaFicaAbaixoDoMinimo(t *testing.T) {
	const minimo = 2000 // R$ 20

	for total := int64(1); total <= 50_000; total += 137 {
		max := MaxInstallmentsFor(total, minimo)
		if max == 1 {
			// À vista é a exceção declarada: existe mesmo abaixo do mínimo.
			continue
		}
		valorDaParcela := total / int64(max)
		if valorDaParcela < minimo {
			t.Fatalf("total %d oferece %d× de %d, abaixo do mínimo de %d",
				total, max, valorDaParcela, minimo)
		}
	}
}
