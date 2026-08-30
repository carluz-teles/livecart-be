package integration

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// As URLs abaixo têm a FORMA exata das capturadas do produto 16698952209 na
// conta real em 29/08/2026 — mesmos parâmetros, mesmos prazos. Guardá-las é o
// que impede alguém de "simplificar" a heurística até ela deixar de reconhecer
// a coisa que motivou o arquivo inteiro.
//
// ⚠ O `AWSAccessKeyId` foi REDIGIDO. As URLs de imagem do Bling carregam a
// chave de acesso AWS DELES, e o push protection do GitHub barra o commit —
// corretamente. Vale a mesma cautela em log: a auditoria do bling-lab e o
// integration_logs guardam URL inteira, e essas URLs levam credencial de
// terceiro junto.
const (
	urlBlingCheia = "https://orgbling.s3.amazonaws.com/0630ce45b74747c4aba7729ac924f48d/" +
		"0138446d55a498b44f682f447063bdd9?AWSAccessKeyId=AKIAEXEMPLOREDIGIDO00" +
		"&Expires=1788651842&Signature=8bfGgttK9BpVvKd7XEa4lJ59jK8%3D"

	urlBlingMiniatura = "https://orgbling.s3.amazonaws.com/0630ce45b74747c4aba7729ac924f48d/t/" +
		"0138446d55a498b44f682f447063bdd9?AWSAccessKeyId=AKIAEXEMPLOREDIGIDO00" +
		"&Expires=1788048842&Signature=iXBSEj9lEs1XBmtJ%2Fq0CnvYTey4%3D"
)

// A heurística tem de acertar nos dois sentidos. Um falso NEGATIVO é o caro:
// a foto do produto some da vitrine em uma semana e ninguém liga a causa.
func TestURLEfemeraReconheceOQueExpira(t *testing.T) {
	casos := []struct {
		nome string
		url  string
		quer bool
	}{
		{"imagem cheia do Bling (medida, expira em 7 dias)", urlBlingCheia, true},
		{"miniatura do Bling (medida, expira em 30 min)", urlBlingMiniatura, true},
		{"S3 SigV4", "https://b.s3.amazonaws.com/x.jpg?X-Amz-Expires=900&X-Amz-Signature=abc", true},

		{"URL do lojista — NÃO copiar", "https://loja.com.br/fotos/vestido.jpg", false},
		{"CDN sem assinatura", "https://cdn.exemplo.com/p/1.png?v=3", false},
		{"Expires SEM Signature não é assinatura", "https://x.com/a.jpg?Expires=123", false},
		{"vazia", "", false},
		{"não é URL", "isto não é url", false},
		{"caminho relativo", "/fotos/x.jpg", false},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := URLEfemera(c.url); got != c.quer {
				t.Errorf("URLEfemera = %v, queria %v", got, c.quer)
			}
		})
	}
}

// O prazo tem de ser legível: a diferença entre "url efêmera" e "esta foto
// morre em 28 minutos" é a diferença entre ruído e alerta acionável.
func TestExpiraEmLeOPrazoReal(t *testing.T) {
	quando, ok := ExpiraEm(urlBlingCheia)
	if !ok {
		t.Fatal("não consegui ler o prazo da URL medida")
	}
	// 1788651842 = 05/09/2026 20:44:02 -03
	if quando.Unix() != 1788651842 {
		t.Errorf("prazo = %d, queria 1788651842", quando.Unix())
	}

	mini, ok := ExpiraEm(urlBlingMiniatura)
	if !ok {
		t.Fatal("não consegui ler o prazo da miniatura")
	}
	// A miniatura vence MUITO antes da imagem cheia — é o detalhe que faz a
	// grade de variações quebrar primeiro que o produto.
	if !mini.Before(quando) {
		t.Error("a miniatura devia vencer antes da imagem cheia")
	}
	if d := quando.Sub(mini); d < 6*24*time.Hour {
		t.Errorf("a distância entre os dois prazos é %s — a medição dizia ~7 dias contra ~30 min", d)
	}

	if _, ok := ExpiraEm("https://loja.com/x.jpg"); ok {
		t.Error("URL sem Expires não pode devolver prazo")
	}
}

// O nome do arquivo NÃO pode derivar da query: ela carrega a assinatura, que
// muda a cada leitura. Um nome derivado dela faria a MESMA foto virar um
// arquivo novo a cada import, enchendo o bucket de duplicatas.
func TestNomeDoArquivoIgnoraAAssinatura(t *testing.T) {
	a := nomeDeArquivoDaImagem(urlBlingCheia, "16698952209", "image/jpeg")

	// Mesma foto, assinatura diferente (é o que acontece na leitura seguinte).
	outraLeitura := urlBlingCheia + "&X-Extra=1"
	b := nomeDeArquivoDaImagem(outraLeitura, "16698952209", "image/jpeg")

	if a != b {
		t.Errorf("a mesma foto gerou nomes diferentes (%q vs %q) — o bucket encheria de duplicatas", a, b)
	}
	if want := "16698952209-0138446d55a498b44f682f447063bdd9.jpg"; a != want {
		t.Errorf("nome = %q, queria %q", a, want)
	}
}

func TestNomeDoArquivoGanhaExtensaoDoTipo(t *testing.T) {
	casos := map[string]string{
		"image/png":  ".png",
		"image/webp": ".webp",
		"image/gif":  ".gif",
		"image/jpeg": ".jpg",
		"":           ".jpg", // sem tipo confiável, jpeg é o que os ERPs mandam
	}
	for tipo, querExt := range casos {
		got := nomeDeArquivoDaImagem("https://x.com/sem-extensao", "p1", tipo)
		if len(got) < len(querExt) || got[len(got)-len(querExt):] != querExt {
			t.Errorf("com tipo %q o nome ficou %q, queria terminar em %q", tipo, got, querExt)
		}
	}
}

// Sem storage configurado, a re-hospedagem tem de devolver a URL ORIGINAL e
// seguir. Perder a foto seria pior do que uma foto com prazo — e derrubar o
// import por causa dela seria pior ainda.
func TestSemStorageDevolveAOriginalSemQuebrar(t *testing.T) {
	s := &Service{logger: zap.NewNop()}

	if got := s.ReHospedarImagem(t.Context(), "loja-1", "p1", urlBlingCheia); got != urlBlingCheia {
		t.Errorf("devolveu %q, queria a original", got)
	}
	if got := s.ReHospedarImagem(t.Context(), "loja-1", "p1", ""); got != "" {
		t.Errorf("URL vazia devia continuar vazia, veio %q", got)
	}
}

// URL estável NUNCA é copiada, mesmo com storage disponível: re-hospedar o que
// já é estável gasta armazenamento e cria uma segunda cópia para desatualizar.
func TestURLEstavelNaoEhCopiada(t *testing.T) {
	s := &Service{logger: zap.NewNop()}
	const doLojista = "https://loja.com.br/fotos/vestido.jpg"

	if got := s.ReHospedarImagem(t.Context(), "loja-1", "p1", doLojista); got != doLojista {
		t.Errorf("copiou uma URL estável: %q", got)
	}
}

// A ORDEM da lista importa: a primeira é a imagem principal do produto, e
// trocá-la mudaria a foto que a compradora vê sem ninguém ter pedido.
func TestReHospedarPreservaAOrdemDaLista(t *testing.T) {
	s := &Service{logger: zap.NewNop()}
	entrada := []string{
		"https://loja.com/1.jpg",
		"https://loja.com/2.jpg",
		"https://loja.com/3.jpg",
	}
	saida := s.ReHospedarImagens(t.Context(), "loja-1", "p1", entrada)

	if len(saida) != len(entrada) {
		t.Fatalf("entrou %d, saiu %d", len(entrada), len(saida))
	}
	for i := range entrada {
		if saida[i] != entrada[i] {
			t.Errorf("posição %d mudou: %q → %q", i, entrada[i], saida[i])
		}
	}
}

// Lista vazia e nil não podem virar slice vazio "por conveniência": o
// chamador distingue "não tem imagem" de "tem zero imagens".
func TestReHospedarListaVaziaNaoInventaSlice(t *testing.T) {
	s := &Service{logger: zap.NewNop()}
	if got := s.ReHospedarImagens(t.Context(), "loja-1", "p1", nil); got != nil {
		t.Errorf("nil virou %v", got)
	}
}
