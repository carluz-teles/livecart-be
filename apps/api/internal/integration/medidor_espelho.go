package integration

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// MEDIÇÃO DO ESPELHO DE ESTOQUE CONTRA O TETO DO ERP.
//
// Em 02/09/2026, 202 das 301 linhas de erro/aviso de produção eram 429 do Tiny,
// e 100% delas vinham do espelho de estoque. A varredura de pedidos — que era o
// outro suspeito — tinha acabado de ser corrigida e apareceu ZERO vez.
//
// A pergunta que sobrou não tem resposta nos logs de hoje, e é ela que decide o
// conserto:
//
//	CHUVA  o volume real de webhooks distintos é alto demais para o teto?
//	ECO    o volume é aceitável e o que satura são as RETENTATIVAS?
//
// As duas produzem a mesma pilha de 429 no log, e pedem correções opostas: a
// chuva se resolve confiando mais no payload ou agrupando por janela (as duas
// custam precisão do estoque durante a live); o eco se resolve calibrando
// backoff, que não custa nada a ninguém.
//
// Contar é barato e não muda comportamento nenhum. Por isso isto entra antes de
// qualquer decisão, e não depois.
//
// ═══ COMO LER O RESUMO ═══
//
//	espelho de estoque: um minuto  recebidos=41 coalescidos=28 lidos=13
//	                               retentativas=22 limitados=19 produtos=9
//
//	recebidos     webhooks que chegaram do ERP           ← a chuva
//	coalescidos   absorvidos por leitura já em curso     ← o que já economizamos
//	lidos         leituras REALMENTE disparadas          ← o custo contra o teto
//	retentativas  releituras por falha da anterior       ← o eco
//	limitados     429 recebidos do provedor
//	produtos      produtos distintos no minuto
//
// `lidos` é o número que importa: é ele que disputa os 30/min com a venda. Se
// `lidos` couber no teto e `limitados` for alto, o problema é o eco.

// medidorDoEspelho acumula os contadores de UM minuto e os despeja numa linha
// quando o minuto vira.
//
// Sem persistência e sem métrica externa de propósito: o objetivo é uma
// resposta, não um painel. Quando ela vier, isto sai.
type medidorDoEspelho struct {
	mu       sync.Mutex
	janela   time.Time
	produtos map[string]struct{}

	recebidos    int
	coalescidos  int
	lidos        int
	retentativas int
	limitados    int
}

func novoMedidorDoEspelho() *medidorDoEspelho {
	return &medidorDoEspelho{produtos: map[string]struct{}{}}
}

// registrar soma um evento e devolve o resumo do minuto ANTERIOR quando a
// virada acontece — nil no resto do tempo.
//
// Devolver em vez de logar aqui dentro mantém a trava curta: quem chama loga
// fora da seção crítica.
func (m *medidorDoEspelho) registrar(tipo, produto string) []zap.Field {
	if m == nil {
		return nil
	}
	agora := time.Now().Truncate(time.Minute)

	m.mu.Lock()
	defer m.mu.Unlock()

	var resumo []zap.Field
	if !m.janela.Equal(agora) {
		if !m.janela.IsZero() && m.recebidos+m.lidos+m.limitados > 0 {
			resumo = m.campos()
		}
		m.janela = agora
		m.produtos = map[string]struct{}{}
		m.recebidos, m.coalescidos, m.lidos, m.retentativas, m.limitados = 0, 0, 0, 0, 0
	}

	switch tipo {
	case "recebido":
		m.recebidos++
	case "coalescido":
		m.coalescidos++
	case "lido":
		m.lidos++
	case "retentativa":
		m.retentativas++
	case "limitado":
		m.limitados++
	}
	if produto != "" {
		m.produtos[produto] = struct{}{}
	}
	return resumo
}

func (m *medidorDoEspelho) campos() []zap.Field {
	return []zap.Field{
		zap.Time("minuto", m.janela),
		zap.Int("recebidos", m.recebidos),
		zap.Int("coalescidos", m.coalescidos),
		zap.Int("lidos", m.lidos),
		zap.Int("retentativas", m.retentativas),
		zap.Int("limitados", m.limitados),
		zap.Int("produtos", len(m.produtos)),
	}
}

// anotarEspelho registra o evento e, na virada do minuto, publica o resumo.
func (s *Service) anotarEspelho(ctx context.Context, tipo, produto string) {
	if s.medidorEspelho == nil {
		return
	}
	if resumo := s.medidorEspelho.registrar(tipo, produto); resumo != nil {
		logger.From(ctx, s.logger).Info("espelho de estoque: um minuto", resumo...)
	}
}
