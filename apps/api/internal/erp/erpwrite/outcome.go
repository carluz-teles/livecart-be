// Package erpwrite é o pipeline de escrita no ERP: classifica desfechos,
// serializa por pedido e respeita o teto real da API.
//
// Ele existe por causa de um número medido em 26/08/2026. Numa live simulada de
// 150 comentários, 170 reservas de estoque foram tentadas: 55 confirmaram e 115
// terminaram em `unconfirmed` — o estado que nunca re-tenta e trava a
// finalização de um carrinho PAGO. Nenhuma delas levou 429. Todas morreram no
// prazo de 90 s ESPERANDO A FILA INTERNA, sem nunca terem sido despachadas.
//
// Uma requisição que nunca saiu da máquina é PROVADAMENTE não aplicada, e é
// seguro repetir. Tratá-la como ambígua é o que trava dois terços de uma live.
// Distinguir "não despachada" de "em voo" é a razão de ser deste pacote.
package erpwrite

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"

	"livecart/apps/api/lib/ratelimit"
)

// Outcome é a classificação de uma tentativa de escrita. É deliberadamente
// ternária: o meio-termo é real e colapsá-lo em sucesso ou falha é o que produz
// tanto estoque fantasma quanto carrinho travado.
type Outcome int

const (
	// Applied — o ERP confirmou. Não repetir.
	Applied Outcome = iota

	// NotApplied — PROVADO que o ERP não aplicou nada. Repetir é seguro, e em
	// vários casos é a única cura (429, e a requisição que nunca saiu da fila).
	NotApplied

	// Unknown — a requisição foi despachada e o desfecho se perdeu. Pode ter
	// sido aplicada. Repetir cegamente duplica; só evidência externa decide.
	Unknown
)

func (o Outcome) String() string {
	switch o {
	case Applied:
		return "applied"
	case NotApplied:
		return "not_applied"
	default:
		return "unknown"
	}
}

// Retryable diz se o desfecho autoriza repetição automática.
func (o Outcome) Retryable() bool { return o == NotApplied }

// ErrNotDispatched marca a requisição que morreu antes de sair — prazo estourou
// na fila, contexto cancelado antes do envio, limiter recusou. É o sentinela que
// resolve os 115 `unconfirmed` da live simulada.
var ErrNotDispatched = errors.New("erpwrite: request was never dispatched")

// Attempt é o que se sabe sobre uma tentativa depois de executada.
type Attempt struct {
	Dispatched bool // saiu da máquina?
	StatusCode int  // 0 quando não houve resposta
	Err        error
}

// Classify traduz uma tentativa em desfecho. É a ÚNICA tabela de classificação
// do sistema; qualquer caminho que decida por conta própria acaba divergindo,
// que foi exatamente como o estorno passou meses classificando 429 como
// ambíguo.
func Classify(a Attempt) Outcome {
	// Nunca despachada: nada chegou ao ERP, aconteça o que for no erro.
	if !a.Dispatched || errors.Is(a.Err, ErrNotDispatched) {
		return NotApplied
	}

	if a.StatusCode > 0 {
		switch {
		case a.StatusCode >= 200 && a.StatusCode < 300:
			return Applied
		case a.StatusCode == http.StatusTooManyRequests:
			// Recusa por rate limit: o ERP disse não ANTES de processar.
			return NotApplied
		case a.StatusCode >= 400 && a.StatusCode < 500:
			// Recusa de validação: processou e disse não, sem aplicar.
			return NotApplied
		default:
			// 5xx: respondeu, e pode ter aplicado antes de quebrar.
			return Unknown
		}
	}

	// Despachada, sem status e sem erro: a operação completou. Vale para o
	// caminho que não expõe status (uma chamada interna, um fake de teste) —
	// e classificar isso como ambíguo travaria carrinho pago à toa. Este ramo
	// foi acrescentado depois de a suíte de corridas flagrar exatamente isso.
	if a.Err == nil {
		return Applied
	}

	// A recusa do limitador vem ANTES do exame de timeout, e tem de vir: ela
	// EMBRULHA DeadlineExceeded, então cairia no ramo do ambíguo. Mas ela não é
	// ambígua — nenhum byte saiu, e é isso que a torna segura de repetir.
	if errors.Is(a.Err, ratelimit.ErrNaoDespachado) {
		return NotApplied
	}

	// Sem status, com erro: erro de transporte. Só falha de DISCAGEM prova
	// não-entrega; timeout não prova nada, porque o ERP pode ter processado e
	// demorado a responder.
	if dialFailure(a.Err) {
		return NotApplied
	}
	return Unknown
}

// dialFailure é verdadeiro quando o erro prova que nenhum byte foi processado.
// Timeout fica de fora DE PROPÓSITO: é o caso genuinamente ambíguo.
func dialFailure(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH)
}

// Describe explica a classificação em uma linha, para log e painel.
func Describe(a Attempt) string {
	o := Classify(a)
	switch {
	case !a.Dispatched:
		return fmt.Sprintf("%s: nunca despachada (%v)", o, a.Err)
	case a.StatusCode > 0:
		return fmt.Sprintf("%s: HTTP %d", o, a.StatusCode)
	default:
		return fmt.Sprintf("%s: erro de transporte (%v)", o, a.Err)
	}
}
