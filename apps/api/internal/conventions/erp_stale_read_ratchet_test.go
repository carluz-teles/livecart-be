package conventions

// Catraca da leitura vencida do ERP.
//
// A trava otimista (`erp_seq`) recusa o saldo do ERP quando um movimento nosso
// aterrissou entre a leitura e a escrita. Recusar é certo — aquele número
// descreve um passado. Mas recusar NÃO é o fim do assunto: a leitura vencida
// costuma carregar a única notícia que não temos de outra fonte, que é a venda
// do lojista em outro canal.
//
// Durante meses o código descartava e desistia, com um comentário apostando no
// "próximo webhook ou na reconciliação". As duas apostas falham no mesmo caso:
// se aquela venda foi o último movimento do lojista, não vem próximo webhook —
// e a reconciliação só reporta a divergência, não a conserta.
//
// A medida está em `TestLiveQuenteNaoCegaParaOEcommerce`: numa live com
// movimento constante no mesmo SKU, 75% das leituras vencem na primeira
// tentativa. Descartar sem reler significava perder três de cada quatro
// notícias do e-commerce.
//
// Esta catraca existe porque a correção é uma linha fácil de perder num
// refactor: basta alguém "simplificar" o loop de volta para `if err == nil {
// return }` e o buraco reabre em silêncio — sem teste vermelho, sem log de
// erro, só estoque errado semanas depois.

import (
	"os"
	"strings"
	"testing"
)

const caminhoDoSyncDoERP = "../integration/service.go"

// A leitura vencida tem de ter um destino diferente do sucesso e do "produto
// não importado". Enquanto os três couberem num booleano, o loop não consegue
// distinguir "não apliquei porque não havia o que aplicar" de "não apliquei
// porque o número envelheceu na minha mão".
func TestLeituraVencidaDoERPTemDesfechoProprio(t *testing.T) {
	fonte := lerFonte(t, caminhoDoSyncDoERP)

	for _, nome := range []string{"stockMirrorApplied", "stockMirrorNoTarget", "stockMirrorStale"} {
		if !strings.Contains(fonte, nome) {
			t.Fatalf("o desfecho %q sumiu de service.go — sem ele o loop volta a tratar "+
				"'nada a aplicar' e 'leitura envelheceu' como a mesma coisa, e a segunda "+
				"para de ser retentada", nome)
		}
	}
}

// O desfecho tem de mudar o comportamento, e não só existir. Sem esta
// verificação, `stockMirrorStale` pode virar um enum decorativo: atribuído no
// ponto do descarte, ignorado por quem chama.
func TestLeituraVencidaProvocaOutraRodada(t *testing.T) {
	fonte := lerFonte(t, caminhoDoSyncDoERP)

	// O loop de retentativa não pode encerrar a rodada quando o desfecho é
	// vencido. A forma exata pode mudar num refactor; o que não pode mudar é o
	// desfecho vencido aparecer na decisão de continuar.
	if !strings.Contains(fonte, "outcome != stockMirrorStale") {
		t.Error("o loop de ProcessProductWebhook não consulta mais stockMirrorStale antes " +
			"de encerrar. Se ele voltar a sair na primeira passada, a leitura vencida é " +
			"descartada sem releitura — e a venda do lojista em outro canal some, em " +
			"silêncio, exatamente como antes da correção")
	}

	// Esgotar as tentativas ainda vencido é perda real de informação, e tem de
	// gritar. Warn some no volume de uma live; este caso precisa de Error para
	// virar alerta e gancho de reconciliação.
	if !strings.Contains(fonte, `Error("ERP balance never landed`) {
		t.Error("sumiu o log de nível Error para 'venceu em todas as tentativas'. Esse é o " +
			"único aviso de que um saldo do ERP nunca entrou; rebaixá-lo a Warn o esconde " +
			"no volume da live")
	}
}

// O motivo de a releitura funcionar: cada rodada faz um GetProduct NOVO. Se
// alguém mover a consulta ao ERP para fora do trecho retentado — em nome de
// "economizar chamada" — a retentativa passa a reaplicar o mesmo número velho,
// que vai vencer de novo, para sempre.
func TestCadaRodadaLeOERPDeNovo(t *testing.T) {
	fonte := lerFonte(t, caminhoDoSyncDoERP)

	corpo := trechoEntre(t, fonte,
		"func (s *Service) processProductSync(",
		"\n}\n")

	if !strings.Contains(corpo, "erpProvider.GetProduct(ctx, externalProductID)") {
		t.Error("processProductSync não consulta mais o ERP diretamente. A retentativa só " +
			"corrige o contador porque traz um saldo NOVO; reaproveitar o saldo da rodada " +
			"anterior transforma o retry em repetição do mesmo erro")
	}

	// O comprovante tem de ser tirado ANTES da consulta. Lido depois, ele não
	// cobre a ida-e-volta HTTP — que é justamente onde a reserva de outro
	// comprador cabe.
	posSeq := strings.Index(corpo, "ProductSeqByExternalID")
	posGet := strings.Index(corpo, "erpProvider.GetProduct")
	if posSeq < 0 || posGet < 0 {
		t.Fatal("não achei as duas chamadas em processProductSync")
	}
	if posSeq > posGet {
		t.Error("o seq passou a ser lido DEPOIS da consulta ao ERP. Assim ele deixa de " +
			"cobrir a ida-e-volta HTTP, que dura centenas de milissegundos — e um " +
			"movimento nosso nessa fresta passa a ser aceito como se a leitura ainda " +
			"fosse do presente")
	}
}

func lerFonte(t *testing.T, caminho string) string {
	t.Helper()
	b, err := os.ReadFile(caminho)
	if err != nil {
		t.Fatalf("lendo %s: %v", caminho, err)
	}
	return string(b)
}

func trechoEntre(t *testing.T, fonte, inicio, fim string) string {
	t.Helper()
	i := strings.Index(fonte, inicio)
	if i < 0 {
		t.Fatalf("não achei %q", inicio)
	}
	j := strings.Index(fonte[i:], fim)
	if j < 0 {
		t.Fatalf("não achei o fim de %q", inicio)
	}
	return fonte[i : i+j]
}
