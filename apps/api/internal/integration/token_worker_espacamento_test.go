package integration

import (
	"testing"
	"time"
)

// O espaçamento entre renovações existe por um motivo específico e caro: o
// Bling bloqueia o IP por 60 MINUTOS depois de 20 chamadas a /oauth/token em
// 60 segundos, e o IP é o NAT compartilhado do Railway. Um laço nu com 21 lojas
// Bling vencendo na mesma janela derrubaria a frota inteira por uma hora —
// ninguém renova E ninguém consegue conectar.

func TestEspacamentoRespeitaOTetoMaisApertadoDaLeva(t *testing.T) {
	casos := []struct {
		nome       string
		providers  []string
		querMinimo time.Duration
		querZero   bool
	}{
		{
			nome:      "só providers sem teto conhecido — não espaça",
			providers: []string{"tiny", "mercado_pago", "instagram"},
			querZero:  true,
		},
		{
			nome:       "uma loja Bling na leva já impõe o espaçamento",
			providers:  []string{"tiny", "bling", "instagram"},
			querMinimo: 6 * time.Second, // 60s / 10
		},
		{
			nome:       "leva só de Bling",
			providers:  []string{"bling", "bling", "bling"},
			querMinimo: 6 * time.Second,
		},
		{
			nome:      "leva vazia",
			providers: nil,
			querZero:  true,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			var integracoes []IntegrationRow
			for _, p := range c.providers {
				integracoes = append(integracoes, IntegrationRow{Provider: p})
			}

			got := espacamentoEntreRenovacoes(integracoes)
			if c.querZero {
				if got != 0 {
					t.Errorf("espaçamento = %s, queria 0 — espaçar sem necessidade "+
						"atrasa a renovação de quem não tem teto", got)
				}
				return
			}
			if got != c.querMinimo {
				t.Errorf("espaçamento = %s, queria %s", got, c.querMinimo)
			}
		})
	}
}

// O teto do Bling é metade do que a doc permite, e isso é deliberado: o MESMO
// endpoint atende o authorization_code. Gastar a cota toda renovando impediria
// um lojista de CONECTAR.
func TestTetoDoBlingDeixaFolgaParaQuemEstaConectando(t *testing.T) {
	teto, tem := tetoDeTokensPorMinuto["bling"]
	if !tem {
		t.Fatal("o Bling saiu da tabela de tetos — sem ele o worker volta a disparar em rajada")
	}
	// A doc do Bling diz 20 por 60 s. Usar os 20 não deixaria vaga para o
	// authorization_code de quem está conectando naquele minuto.
	if teto >= 20 {
		t.Errorf("teto = %d, e a doc do Bling permite 20 no MESMO endpoint que atende "+
			"o authorization_code — sem folga, renovar impede conectar", teto)
	}
	if teto <= 0 {
		t.Errorf("teto = %d — um teto não-positivo desliga o espaçamento", teto)
	}
}

// Uma leva de 21 lojas Bling é o cenário que motivou tudo: com o laço nu ela
// estouraria as 20 chamadas em muito menos de 60 s.
func TestVinteEUmaLojasBlingLevamMaisDeUmMinuto(t *testing.T) {
	const lojas = 21
	var integracoes []IntegrationRow
	for i := 0; i < lojas; i++ {
		integracoes = append(integracoes, IntegrationRow{Provider: "bling"})
	}

	espacamento := espacamentoEntreRenovacoes(integracoes)
	// A primeira não espera; as outras 20 esperam.
	total := time.Duration(lojas-1) * espacamento

	if total < time.Minute {
		t.Errorf("as %d renovações levariam %s — precisam passar de 1 minuto, "+
			"senão as 20 primeiras caem na mesma janela e o IP é bloqueado por 60 min",
			lojas, total)
	}
}
