package erp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// O defeito que este arquivo prova e trava.
//
// Enquanto o fluxo de ERP perguntava por `GetActiveByProvider(ctx, storeID,
// "erp", "tiny")` literal, uma loja com QUALQUER outro ERP recebia not-found —
// e quatro guards traduziam not-found como "loja sem ERP", devolvendo nil sem
// erro e sem log em nível visível. A consequência não é um erro: é uma live
// inteira rodando, vendendo, e nenhum pedido nascendo no ERP. Silêncio, que é
// a pior falha possível — a mesma classe do subscribed_apps do Instagram.
//
// Hoje isso é impossível em produção porque o CHECK do banco só aceitava
// 'tiny'. A migration 000145 abre o CHECK para 'bling' e, no instante em que
// abre, CRIA a possibilidade. Por isso os dois têm de ir no mesmo deploy, e por
// isso este teste existe: ele falha se alguém reintroduzir o literal.

func repoComERP(t *testing.T, provider string) *repoSimulado {
	t.Helper()
	repo := novoRepoSimulado()
	repo.provider = provider
	return repo
}

// A loja tem Bling. O fluxo tem de ENXERGAR o ERP dela.
func TestLojaComBlingNaoEhTratadaComoLojaSemERP(t *testing.T) {
	casos := []string{"tiny", "bling"}
	for _, provider := range casos {
		t.Run(provider, func(t *testing.T) {
			repo := repoComERP(t, provider)

			integ, err := repo.GetActiveERP(context.Background(), "loja-1")
			if err != nil {
				t.Fatalf("GetActiveERP devolveu erro para uma loja COM ERP %q: %v", provider, err)
			}
			if integ == nil {
				t.Fatalf("GetActiveERP devolveu nil para uma loja COM ERP %q", provider)
			}
			if integ.Provider != provider {
				t.Errorf("provider resolvido = %q, queria %q", integ.Provider, provider)
			}
		})
	}
}

// Loja SEM ERP nenhum continua sendo not-found — o comportamento de "pular"
// tem de sobreviver à mudança, senão consertar o Bling quebra quem não tem ERP.
func TestLojaSemERPContinuaSendoNotFound(t *testing.T) {
	repo := novoRepoSimulado()
	repo.semIntegracao = true

	if _, err := repo.GetActiveERP(context.Background(), "loja-1"); err == nil {
		t.Fatal("loja sem ERP devia devolver erro (not-found), devolveu nil")
	}
}

// TESTE-GUARDA. Nenhum arquivo do fluxo de ERP pode voltar a perguntar por
// provider literal.
//
// É teste de FONTE, e isso é deliberado: o defeito não é uma chamada errada que
// um teste de comportamento pegaria — é uma pergunta certa feita ao objeto
// errado, que devolve "não achei" e vira silêncio. A única forma barata de
// impedir a reintrodução é proibir o literal.
func TestNenhumCallSiteDeERPPerguntaPorProviderLiteral(t *testing.T) {
	// A proibição é PRECISA: só a RESOLUÇÃO do ERP ativo por provider literal.
	//
	// `GetByProvider(..., "erp", "tiny")` continua legítimo e NÃO é proibido —
	// getTinyOAuthURL e handleTinyCallback são o fluxo de conexão DO TINY, e
	// perguntar pelo Tiny ali é a pergunta certa. O que não pode existir é o
	// fluxo de ERP (pedido, estoque, nota) resolver o provider por literal.
	for _, dir := range []string{".", "../integration"} {
		arquivos, err := arquivosGo(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range arquivos {
			if strings.HasSuffix(f.nome, "_test.go") {
				continue
			}
			for i, linha := range strings.Split(f.conteudo, "\n") {
				if strings.Contains(linha, "//") {
					continue // comentário citando o padrão antigo é documentação, não call site
				}
				// `"erp", "` casa só quando o provider é LITERAL. A forma
				// dinâmica — GetActiveByProvider(ctx, storeID, "erp", provider) —
				// é legítima e fica de fora: ela é o roteamento do webhook, que
				// já recebe o provider certo de quem entregou o evento.
				if strings.Contains(linha, "GetActiveByProvider(") && strings.Contains(linha, `"erp", "`) {
					t.Errorf("%s:%d resolve o ERP por provider literal — use GetActiveERP(storeID).\n"+
						"  linha: %s\n"+
						"  Com um ERP que não seja Tiny isso devolve not-found, e os guards\n"+
						"  traduzem not-found como 'loja sem ERP': a live roda inteira sem\n"+
						"  criar pedido e sem erro no log.", f.nome, i+1, strings.TrimSpace(linha))
				}
			}
		}
	}
}

type arquivoGo struct {
	nome     string
	conteudo string
}

func arquivosGo(dir string) ([]arquivoGo, error) {
	entradas, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []arquivoGo
	for _, e := range entradas {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, arquivoGo{nome: filepath.Join(dir, e.Name()), conteudo: string(b)})
	}
	return out, nil
}
