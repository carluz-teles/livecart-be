package erpwrite

// A tabela de classificação é o núcleo do pacote, então ela é testada de forma
// EXAUSTIVA: todo status HTTP de 100 a 599, cruzado com despachada/não e com
// cada família de erro de transporte. São milhares de subtestes, e o ponto não é
// o número — é que nenhuma faixa de status possa mudar de classe sem alguém
// perceber.
//
// A regra que todos eles protegem: só NotApplied autoriza repetir. Errar para
// NotApplied duplica pedido; errar para Unknown trava carrinho pago.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"livecart/apps/api/lib/ratelimit"
)

func classeEsperada(status int) Outcome {
	switch {
	case status >= 200 && status < 300:
		return Applied
	case status == 429:
		return NotApplied
	case status >= 400 && status < 500:
		return NotApplied
	default:
		return Unknown
	}
}

// TestClassificaTodoStatusDespachado varre 100..599 com a requisição despachada.
func TestClassificaTodoStatusDespachado(t *testing.T) {
	for status := 100; status <= 599; status++ {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			got := Classify(Attempt{Dispatched: true, StatusCode: status})
			want := classeEsperada(status)
			if got != want {
				t.Errorf("status %d classificado como %s, esperava %s", status, got, want)
			}
		})
	}
}

// TestNuncaDespachadaEhSempreNotApplied — a propriedade que resolve os 115
// `unconfirmed` da live simulada. Nada que não saiu da máquina pode ser ambíguo,
// nem quando o status ou o erro sugerem outra coisa.
func TestNuncaDespachadaEhSempreNotApplied(t *testing.T) {
	erros := []struct {
		nome string
		err  error
	}{
		{"nenhum", nil},
		{"deadline", context.DeadlineExceeded},
		{"cancelado", context.Canceled},
		{"sentinela", ErrNotDispatched},
		{"embrulhado", fmt.Errorf("fila: %w", ErrNotDispatched)},
		{"conexao recusada", syscall.ECONNREFUSED},
		{"generico", errors.New("qualquer coisa")},
	}
	for status := 0; status <= 599; status += 7 {
		for _, e := range erros {
			t.Run(fmt.Sprintf("status_%d/%s", status, e.nome), func(t *testing.T) {
				got := Classify(Attempt{Dispatched: false, StatusCode: status, Err: e.err})
				if got != NotApplied {
					t.Errorf("não despachada (status %d, err %v) virou %s; "+
						"o que nunca saiu da máquina é provadamente não aplicado", status, e.err, got)
				}
			})
		}
	}
}

// TestErroDeTransporteSemStatus separa discagem (prova não-entrega) de timeout
// (não prova nada). É a distinção que impede reserva fantasma.
func TestErroDeTransporteSemStatus(t *testing.T) {
	casos := []struct {
		nome string
		err  error
		want Outcome
	}{
		{"conexao recusada", syscall.ECONNREFUSED, NotApplied},
		{"host inalcancavel", syscall.EHOSTUNREACH, NotApplied},
		{"rede inalcancavel", syscall.ENETUNREACH, NotApplied},
		{"dns nao resolveu", &net.DNSError{Err: "no such host", Name: "api.tiny.com.br"}, NotApplied},
		{"recusada embrulhada", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), NotApplied},
		{"deadline do contexto", context.DeadlineExceeded, Unknown},
		{"timeout de rede", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, Unknown},
		{"deadline embrulhado", fmt.Errorf("post: %w", context.DeadlineExceeded), Unknown},
		{"erro opaco", errors.New("algo quebrou"), Unknown},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			got := Classify(Attempt{Dispatched: true, StatusCode: 0, Err: c.err})
			if got != c.want {
				t.Errorf("%v classificado como %s, esperava %s", c.err, got, c.want)
			}
		})
	}
}

// TestSomenteNotAppliedEhRepetivel trava a regra em todas as classes.
func TestSomenteNotAppliedEhRepetivel(t *testing.T) {
	for _, o := range []Outcome{Applied, NotApplied, Unknown} {
		t.Run(o.String(), func(t *testing.T) {
			if got, want := o.Retryable(), o == NotApplied; got != want {
				t.Errorf("Retryable()=%v para %s, esperava %v", got, o, want)
			}
		})
	}
}

// TestClassificacaoEhDeterministica — a mesma tentativa sempre dá a mesma
// classe. Parece óbvio, e é justamente por isso que vale travar: um dia alguém
// coloca relógio ou aleatoriedade aqui.
func TestClassificacaoEhDeterministica(t *testing.T) {
	for status := 200; status <= 599; status += 13 {
		for _, disp := range []bool{true, false} {
			a := Attempt{Dispatched: disp, StatusCode: status, Err: errors.New("x")}
			primeiro := Classify(a)
			for i := 0; i < 25; i++ {
				if got := Classify(a); got != primeiro {
					t.Fatalf("status %d disp=%v: classificação mudou de %s para %s na repetição %d",
						status, disp, primeiro, got, i)
				}
			}
		}
	}
}

// TestDescribeNuncaEntraEmPanico sobre todo o espaço de entrada.
func TestDescribeNuncaEntraEmPanico(t *testing.T) {
	errs := []error{nil, errors.New("x"), context.DeadlineExceeded, ErrNotDispatched, syscall.ECONNREFUSED}
	for status := 0; status <= 599; status += 11 {
		for _, disp := range []bool{true, false} {
			for _, e := range errs {
				if s := Describe(Attempt{Dispatched: disp, StatusCode: status, Err: e}); s == "" {
					t.Errorf("Describe devolveu vazio para status=%d disp=%v err=%v", status, disp, e)
				}
			}
		}
	}
}

// TestDespachadaSemStatusNemErroEhAplicada trava a regra que a suíte de corridas
// descobriu faltando: uma chamada que saiu, completou e não devolveu status
// (caminho interno, fake de teste) é SUCESSO. Classificá-la como ambígua
// travaria um carrinho pago sem que nada tivesse dado errado.
func TestDespachadaSemStatusNemErroEhAplicada(t *testing.T) {
	if got := Classify(Attempt{Dispatched: true, StatusCode: 0, Err: nil}); got != Applied {
		t.Fatalf("classificou %s; despachada, sem erro e sem status é sucesso", got)
	}
}

// TestMatrizCompletaDaClassificacao cobre o produto cartesiano de
// despachada × status × erro, garantindo que nenhuma combinação fique órfã.
func TestMatrizCompletaDaClassificacao(t *testing.T) {
	statuses := []int{0, 200, 201, 204, 301, 400, 401, 403, 404, 409, 422, 429, 500, 502, 503}
	errs := []struct {
		nome string
		err  error
	}{
		{"nil", nil},
		{"discagem", syscall.ECONNREFUSED},
		{"timeout", context.DeadlineExceeded},
		{"nao_despachada", ErrNotDispatched},
		{"opaco", errors.New("x")},
	}
	for _, disp := range []bool{true, false} {
		for _, st := range statuses {
			for _, e := range errs {
				t.Run(fmt.Sprintf("disp_%v/st_%d/%s", disp, st, e.nome), func(t *testing.T) {
					got := Classify(Attempt{Dispatched: disp, StatusCode: st, Err: e.err})
					switch {
					case !disp || errors.Is(e.err, ErrNotDispatched):
						if got != NotApplied {
							t.Errorf("não despachada virou %s", got)
						}
					case st > 0:
						if want := classeEsperada(st); got != want {
							t.Errorf("status %d virou %s, esperava %s", st, got, want)
						}
					case e.err == nil:
						if got != Applied {
							t.Errorf("sucesso sem status virou %s", got)
						}
					}
				})
			}
		}
	}
}

// A recusa do limitador é "NÃO APLICADO", e não "não sei".
//
// É a metade que TEM de vir antes de afrouxar o teto de escrita. Ao dar mais
// vazão ao balde de cima, quem passa a frear é o limitador do provider — e se a
// recusa dele fosse lida como timeout ambíguo, cada live trocaria "algumas
// compradoras falham" por "carrinho pago travado", que é estritamente pior.
func TestRecusaDoLimitadorEhNaoAplicado(t *testing.T) {
	// Dispatched: true de propósito — é o caso REAL. A recusa do limitador do
	// provider acontece DENTRO da chamada, abaixo de quem monta o Attempt, e
	// portanto depois de o chamador já ter marcado que despachou. Sem esta
	// checagem o erro cairia no ramo do timeout ambíguo.
	got := Classify(Attempt{Dispatched: true, Err: ratelimit.ErrNaoDespachado})
	if got != NotApplied {
		t.Errorf("classificou como %v, queria NotApplied — a recusa acontece ANTES "+
			"de qualquer byte sair, e o que não saiu é seguro repetir", got)
	}
}

// E um timeout DE VERDADE continua ambíguo — a correção não pode vazar para ele.
func TestTimeoutDeRedeContinuaAmbiguo(t *testing.T) {
	got := Classify(Attempt{Dispatched: true, Err: context.DeadlineExceeded})
	if got != Unknown {
		t.Errorf("classificou DeadlineExceeded cru como %v, queria Unknown: o ERP "+
			"pode ter processado e demorado a responder", got)
	}
}
