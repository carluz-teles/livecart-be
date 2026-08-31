package conventions

// A NUMERAÇÃO DAS MIGRATIONS, e o defeito silencioso que ela esconde.
//
// golang-migrate guarda UM inteiro por banco e só aplica versões MAIORES que
// ele. Duas consequências que não são óbvias lendo o código:
//
//  1. Dois arquivos com o mesmo número em branches diferentes viram a MESMA
//     versão para o banco. Quem aplicou primeiro decide qual SQL rodou — e os
//     ambientes divergem em silêncio, cada um achando que está migrado.
//
//  2. Uma migration acrescentada com número MENOR que o já aplicado nunca roda.
//     Nem agora, nem nunca: não há erro, não há aviso, a coluna simplesmente
//     não existe.
//
// Os dois aconteceram juntos aqui, em 29/08/2026. O número 139 foi usado por
// `billing_single_plan` e por `erp_order_status_tracking` em branches
// distintos. Produção aplicou a de billing; staging aplicou a de ERP. O commit
// 4bba36d renomeou a de ERP para 144 e recolocou billing como 139 — consertando
// o conflito de ARQUIVO e deixando o de ESTADO intacto. Staging já estava em
// 144, então a 139 de billing nunca rodaria.
//
// Custo: centenas de `column "billing_interval" does not exist` por minuto em
// toda requisição autenticada de staging, durante dois dias, sem quebrar nada
// visivelmente — o guard de billing falha em fail-open. Reparado pela 000146,
// que é idempotente porque precisa alcançar ambiente que já passou do ponto.

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func versoesDeMigration(t *testing.T) (map[int][]string, []int) {
	t.Helper()
	dir := "../../db/migrations"
	entradas, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("lendo %s: %v", dir, err)
	}
	porVersao := map[int][]string{}
	for _, e := range entradas {
		nome := e.Name()
		if !strings.HasSuffix(nome, ".up.sql") {
			continue
		}
		partes := strings.SplitN(nome, "_", 2)
		n, err := strconv.Atoi(partes[0])
		if err != nil {
			t.Errorf("migration %q não começa com número", nome)
			continue
		}
		porVersao[n] = append(porVersao[n], nome)
	}
	versoes := make([]int, 0, len(porVersao))
	for v := range porVersao {
		versoes = append(versoes, v)
	}
	sort.Ints(versoes)
	return porVersao, versoes
}

// Dois arquivos com o mesmo número são a MESMA versão para o banco. Quem
// aplicou primeiro decide qual SQL rodou, e os ambientes divergem em silêncio.
func TestNenhumaMigrationRepeteONumero(t *testing.T) {
	porVersao, versoes := versoesDeMigration(t)
	for _, v := range versoes {
		if arquivos := porVersao[v]; len(arquivos) > 1 {
			t.Errorf("a versão %d tem %d arquivos (%v) — para o banco é UMA versão só, "+
				"e o ambiente que aplicar primeiro decide qual SQL rodou; o outro "+
				"nunca vai rodar o dele", v, len(arquivos), arquivos)
		}
	}
}

// Um buraco na sequência significa que alguém removeu ou renumerou uma
// migration. Renumerar é o movimento que criou o defeito de 29/08: o arquivo
// muda de nome, mas o número velho continua gravado nos bancos que já o
// aplicaram, e o número novo pode ficar abaixo do ponteiro deles.
func TestNumeracaoDeMigrationsNaoTemBuraco(t *testing.T) {
	_, versoes := versoesDeMigration(t)
	if len(versoes) == 0 {
		t.Fatal("nenhuma migration encontrada")
	}
	var buracos []int
	for n := versoes[0]; n <= versoes[len(versoes)-1]; n++ {
		if _, existe := busca(versoes, n); !existe {
			buracos = append(buracos, n)
		}
	}
	if len(buracos) > 0 {
		t.Errorf("faltam as versões %v na sequência — renumerar ou remover migration "+
			"deixa o número antigo gravado nos bancos que já o aplicaram, e o novo "+
			"pode cair ABAIXO do ponteiro deles (aí não roda nunca). Se a renumeração "+
			"for mesmo necessária, acrescente uma migration de REPARO idempotente no "+
			"topo, como a 000146 fez", buracos)
	}
}

// Todo up tem um down. Sem ele um rollback trava no meio.
func TestTodaMigrationTemODown(t *testing.T) {
	porVersao, versoes := versoesDeMigration(t)
	for _, v := range versoes {
		for _, up := range porVersao[v] {
			down := strings.TrimSuffix(up, ".up.sql") + ".down.sql"
			if _, err := os.Stat(filepath.Join("../../db/migrations", down)); err != nil {
				t.Errorf("%s não tem o par %s", up, down)
			}
		}
	}
}

func busca(ordenado []int, alvo int) (int, bool) {
	i := sort.SearchInts(ordenado, alvo)
	if i < len(ordenado) && ordenado[i] == alvo {
		return i, true
	}
	return 0, false
}
