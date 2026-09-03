package product

// TODO CHAMADOR DE CreateProduct PRECISA PASSAR O ID.
//
// `CreateProduct` lista `id` entre as colunas do INSERT porque o id nasce no
// domínio — sem isso, Save devolvia um id que não correspondia a linha
// nenhuma. O efeito colateral é uma armadilha silenciosa: `products.id` TEM
// `DEFAULT gen_random_uuid()`, mas um NULL explícito no INSERT vence o
// DEFAULT. Quem esquece o campo não ganha um id gerado — ganha 23502.
//
// Foi o que aconteceu com o cadastro de variações: o pacote productgroup
// nunca preencheu o campo, e toda criação de grupo (manual E importada do
// ERP) morria em 500 na primeira variação. O compilador não ajuda, porque o
// zero-value de pgtype.UUID é um struct válido.
//
// Esta trava é textual de propósito: ela roda sem banco, sem build tags e sem
// depender de alguém lembrar de escrever o teste de integração.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateProductSempreRecebeID(t *testing.T) {
	raiz := filepath.Join("..", "..")

	var faltando []string
	err := filepath.Walk(raiz, func(caminho string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// O código gerado é o próprio destino da chamada, não um chamador.
			if info.Name() == "sqlc" || info.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(caminho, ".go") {
			return nil
		}
		conteudo, err := os.ReadFile(caminho)
		if err != nil {
			return err
		}

		linhas := strings.Split(string(conteudo), "\n")
		for i, linha := range linhas {
			if !strings.Contains(linha, "CreateProductParams{") {
				continue
			}
			// Varre o literal até fechar. Se `ID:` não aparecer, o INSERT vai
			// mandar NULL e o banco vai recusar.
			temID := false
			profundidade := 0
			for j := i; j < len(linhas); j++ {
				profundidade += strings.Count(linhas[j], "{") - strings.Count(linhas[j], "}")
				if strings.Contains(linhas[j], "ID:") &&
					!strings.Contains(linhas[j], "StoreID:") &&
					!strings.Contains(linhas[j], "GroupID:") &&
					!strings.Contains(linhas[j], "ExternalID:") {
					temID = true
				}
				if profundidade <= 0 {
					break
				}
			}
			if !temID {
				faltando = append(faltando, filepath.ToSlash(caminho)+":"+itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("varrendo o código: %v", err)
	}

	if len(faltando) > 0 {
		t.Fatalf("CreateProductParams sem ID (o INSERT vai mandar NULL e o banco recusa "+
			"com 23502 — use vo.GenerateProductID().ToPgUUID()):\n  %s",
			strings.Join(faltando, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
