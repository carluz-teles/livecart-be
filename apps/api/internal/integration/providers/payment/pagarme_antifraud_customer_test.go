package payment

// A doc do Pagar.me é explícita sobre o que o antifraude precisa receber:
//
//	"Para que o pedido seja analisado pelo antifraude, é imprescindível o envio
//	 das seguintes informações: name, email, phones, document, type, items,
//	 address (ou billing_address)."
//
// e, para contas PSP — que é o nosso caso, por isso o fluxo usa customer_id:
//
//	"é obrigatório preencher todos os campos do objeto customer, incluindo
//	 endereço e telefone."
//
// O risco aqui é silencioso: `buildPagarmeCustomer` monta os campos sob `if`, e
// um campo ausente não vira erro — vira um cliente incompleto que o antifraude
// pontua mal. O pedido é aprovado pela API e recusado na análise, que é
// exatamente o formato da queixa que originou este teste.

import (
	"strings"
	"testing"
)

func TestClienteDoPagarmeLevaTudoQueOAntifraudePede(t *testing.T) {
	customer := buildPagarmeCustomer(CheckoutCustomer{
		Name:     "Maria Souza",
		Email:    "maria@exemplo.com.br",
		Document: "12345678909",
		Phone:    "11987654321",
		Address: &CheckoutAddress{
			ZipCode:      "01310100",
			Street:       "Avenida Paulista",
			Number:       "1000",
			Neighborhood: "Bela Vista",
			City:         "São Paulo",
			State:        "SP",
		},
	})

	for _, campo := range []string{"name", "email", "type", "document", "document_type", "phones", "address"} {
		if _, ok := customer[campo]; !ok {
			t.Errorf("campo %q ausente no customer — o antifraude exige e sem ele a análise recusa", campo)
		}
	}
}

func TestDocumentTypeUsaOEnumDaDoc(t *testing.T) {
	// Os três únicos valores aceitos, em caixa alta. O código mandava "cpf"
	// cravado em minúsculo para qualquer documento.
	casos := []struct {
		nome      string
		documento string
		quer      string
	}{
		{"CPF tem onze dígitos", "12345678909", "CPF"},
		{"CPF formatado continua CPF", "123.456.789-09", "CPF"},
		{"CNPJ tem quatorze dígitos", "12345678000199", "CNPJ"},
		{"CNPJ formatado continua CNPJ", "12.345.678/0001-99", "CNPJ"},
		{"o que não é nenhum dos dois é passaporte", "AB123456", "PASSPORT"},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := pagarmeDocumentType(c.documento); got != c.quer {
				t.Errorf("pagarmeDocumentType(%q) = %q, quero %q", c.documento, got, c.quer)
			}
		})
	}
}

func TestDocumentTypeNuncaSaiEmMinusculo(t *testing.T) {
	// A regressão que este teste existe para impedir é literalmente a linha que
	// estava no código: um valor minúsculo fora do enum documentado.
	for _, doc := range []string{"12345678909", "12345678000199", "AB123456", ""} {
		got := pagarmeDocumentType(doc)
		if got != strings.ToUpper(got) {
			t.Errorf("pagarmeDocumentType(%q) devolveu %q, fora do enum em caixa alta", doc, got)
		}
	}
}
