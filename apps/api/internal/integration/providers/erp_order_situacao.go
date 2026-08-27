package providers

import "errors"

// Situação do pedido no ERP — o vocabulário inteiro, numa lista só.
//
// Os números vêm do enum de `PUT /pedidos/{id}/situacao` (swagger v3.1); os
// slugs vêm do campo `codigoSituacao` que o webhook de venda entrega. São duas
// grafias da MESMA coisa, e o Tiny usa uma em cada canal: quem faz a transição
// manda o número, quem recebe a notificação lê o slug. Manter as duas lado a
// lado aqui é o que evita um `switch` de strings mágicas em cada ponta.
//
// O par foi confirmado item a item em 26/08/2026, passando um pedido de teste
// por todas as dez situações e lendo o webhook que cada uma disparou.
const (
	SituacaoDadosIncompletos = 8
	SituacaoAberta           = 0
	SituacaoAprovada         = 3
	SituacaoPreparandoEnvio  = 4
	SituacaoFaturada         = 1
	SituacaoProntoEnvio      = 7
	SituacaoEnviada          = 5
	SituacaoEntregue         = 6
	SituacaoCancelada        = 2
	SituacaoNaoEntregue      = 9
)

// ERPOrderStatus é a situação do pedido como o LiveCart a guarda. Deliberadamente
// idêntica ao slug do ERP: traduzir para um vocabulário próprio só criaria um
// dicionário a mais para manter, e o lojista lê o mesmo nome nos dois sistemas.
type ERPOrderStatus string

const (
	ERPOrderStatusDadosIncompletos ERPOrderStatus = "dados_incompletos"
	ERPOrderStatusAberto           ERPOrderStatus = "aberto"
	ERPOrderStatusAprovado         ERPOrderStatus = "aprovado"
	ERPOrderStatusPreparandoEnvio  ERPOrderStatus = "preparando_envio"
	ERPOrderStatusFaturado         ERPOrderStatus = "faturado"
	ERPOrderStatusProntoEnvio      ERPOrderStatus = "pronto_envio"
	ERPOrderStatusEnviado          ERPOrderStatus = "enviado"
	ERPOrderStatusEntregue         ERPOrderStatus = "entregue"
	ERPOrderStatusCancelado        ERPOrderStatus = "cancelado"
	ERPOrderStatusNaoEntregue      ERPOrderStatus = "nao_entregue"
)

// situacaoPorStatus é a única tabela; as duas funções abaixo a leem nos dois
// sentidos, para não existir a chance de uma metade contradizer a outra.
var situacaoPorStatus = map[ERPOrderStatus]int{
	ERPOrderStatusDadosIncompletos: SituacaoDadosIncompletos,
	ERPOrderStatusAberto:           SituacaoAberta,
	ERPOrderStatusAprovado:         SituacaoAprovada,
	ERPOrderStatusPreparandoEnvio:  SituacaoPreparandoEnvio,
	ERPOrderStatusFaturado:         SituacaoFaturada,
	ERPOrderStatusProntoEnvio:      SituacaoProntoEnvio,
	ERPOrderStatusEnviado:          SituacaoEnviada,
	ERPOrderStatusEntregue:         SituacaoEntregue,
	ERPOrderStatusCancelado:        SituacaoCancelada,
	ERPOrderStatusNaoEntregue:      SituacaoNaoEntregue,
}

// ERPOrderStatusFromSituacao traduz o número que o `GET /pedidos` devolve.
// ok=false para qualquer código fora do enum — o ERP pode ganhar situações
// novas numa versão futura, e inventar um nome para elas seria pior do que
// admitir que não conhecemos aquela.
func ERPOrderStatusFromSituacao(situacao int) (ERPOrderStatus, bool) {
	for status, code := range situacaoPorStatus {
		if code == situacao {
			return status, true
		}
	}
	return "", false
}

// SituacaoFromERPOrderStatus é o caminho de volta, para quem tem o slug do
// webhook e precisa do número que a transição usa.
func SituacaoFromERPOrderStatus(status ERPOrderStatus) (int, bool) {
	code, ok := situacaoPorStatus[status]
	return code, ok
}

// ParseERPOrderStatus valida um `codigoSituacao` cru vindo do webhook.
func ParseERPOrderStatus(codigo string) (ERPOrderStatus, bool) {
	status := ERPOrderStatus(codigo)
	_, ok := situacaoPorStatus[status]
	return status, ok
}

// Terminal diz se o pedido chegou a um estágio de onde não sai sozinho. Usado
// para parar de reconciliar pedidos que já acabaram.
func (s ERPOrderStatus) Terminal() bool {
	return s == ERPOrderStatusEntregue || s == ERPOrderStatusCancelado || s == ERPOrderStatusNaoEntregue
}

// FechadoParaNovosItens diz se o pedido já virou documento fiscal e por isso não
// recebe mais item.
//
// Este é o limite que o lojista reconhece: enquanto está "Em aberto" ou
// "Aprovado" — pago, mas ainda não faturado — ele soma a compra de hoje na de
// ontem e manda tudo numa caixa só. A partir do documento fiscal, somar item é
// emitir nota errada.
//
// "Preparando envio" está do lado fechado, e isso NÃO se lê pelo nome. O nome
// sugere alguém montando a caixa, e a lista do enum o coloca antes de "Faturada".
// Na operação é o contrário: o pedido só entra em preparo depois de a nota sair,
// então quando ele chega aqui o documento fiscal já existe. Regra do lojista,
// 26/08/2026 — não invente uma janela de edição que a nota já fechou.
//
// O ERP não impõe nada disso: em 26/08/2026 a API aceitou (204) editar os itens
// de um pedido em situação "Faturada". A recusa tem de ser nossa.
//
// Cancelado entra na lista pelo motivo oposto: não há pedido vivo a que somar.
func (s ERPOrderStatus) FechadoParaNovosItens() bool {
	switch s {
	case ERPOrderStatusPreparandoEnvio, ERPOrderStatusFaturado, ERPOrderStatusProntoEnvio,
		ERPOrderStatusEnviado, ERPOrderStatusEntregue, ERPOrderStatusNaoEntregue,
		ERPOrderStatusCancelado:
		return true
	}
	return false
}

// ErrOrderStockLaunched é a recusa do ERP em editar um pedido cujo estoque foi
// lançado — na prática, alguém mexeu no pedido pelo painel enquanto a live
// rolava. Chega como `400 {"detalhes":[{"campo":"pedido.motivosBloqueio[0]",
// "mensagem":"estoque lançado"}]}` e é o ÚNICO sinal disponível: o
// `GET /pedidos/{id}` não conta se o estoque foi lançado.
//
// É tipada porque autoriza a única chamada de estorno que sobrou no sistema.
// Confundir esta recusa com um erro qualquer, e estornar por precaução, infla a
// reserva de forma irreversível — ver a nota em ReverseOrderStock.
var ErrOrderStockLaunched = errors.New("pedido bloqueado para edição: estoque lançado")
