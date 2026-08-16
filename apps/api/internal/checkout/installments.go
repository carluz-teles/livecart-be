package checkout

// A regra de quantas parcelas o comprador pode escolher.
//
// Vive aqui, e não no formulário do checkout, porque é regra de NEGÓCIO da loja
// e precisa valer nos dois sentidos: o que a tela oferece e o que o servidor
// aceita. Enquanto morava só no front — uma lista fixa de 1 a 12 — não havia
// nada impedindo um POST direto com `installments: 12` numa venda de R$ 4,90.
//
// Também não dá para delegar ao Pagar.me. A API transparente que usamos aceita
// apenas `installments: N` no objeto credit_card; `installments_setup`, que traz
// `amount` (parcela mínima) e `free_installments`, existe só no Checkout
// hospedado e nos Links de pagamento. No fluxo transparente quem decide é o
// integrador.

// MaxInstallmentsCeiling é o teto absoluto, independente do valor.
//
// Doze é o que o mercado brasileiro pratica e o que a validação da borda já
// aceitava; a parcela mínima só encurta essa lista, nunca a estende.
const MaxInstallmentsCeiling = 12

// MaxInstallmentsFor devolve quantas parcelas cabem num total, respeitando o
// mínimo por parcela configurado pela loja.
//
// `minInstallmentCents` zero significa "sem mínimo" — o comportamento de sempre.
// O resultado nunca é menor que 1: à vista tem de existir mesmo quando o total é
// menor que o mínimo configurado, senão uma venda de R$ 4,90 numa loja com
// mínimo de R$ 20 ficaria sem forma de pagar.
func MaxInstallmentsFor(totalCents int64, minInstallmentCents int) int {
	if minInstallmentCents <= 0 {
		return MaxInstallmentsCeiling
	}
	if totalCents <= 0 {
		return 1
	}
	n := int(totalCents / int64(minInstallmentCents))
	if n < 1 {
		return 1
	}
	if n > MaxInstallmentsCeiling {
		return MaxInstallmentsCeiling
	}
	return n
}
