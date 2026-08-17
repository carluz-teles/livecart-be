package domain

import "time"

// SeenHandle é um arroba que já mandou alguma mensagem nas lives da loja.
//
// Não é um Customer: a plateia inclui quem só comentou e nunca comprou — e o
// caso que motivou a busca é exatamente esse, a conta secundária da própria
// loja que instrui a audiência e acaba gerando pedido no nome dela.
type SeenHandle struct {
	handle            string
	messageCount      int
	orderMessageCount int
	lastSeenAt        *time.Time
	blocked           bool
}

// ReconstructSeenHandle rebuilds a SeenHandle from persistence.
func ReconstructSeenHandle(
	handle string,
	messageCount int,
	orderMessageCount int,
	lastSeenAt *time.Time,
	blocked bool,
) *SeenHandle {
	return &SeenHandle{
		handle:            handle,
		messageCount:      messageCount,
		orderMessageCount: orderMessageCount,
		lastSeenAt:        lastSeenAt,
		blocked:           blocked,
	}
}

func (h *SeenHandle) Handle() string { return h.handle }

// MessageCount são todas as mensagens do arroba nas lives da loja.
func (h *SeenHandle) MessageCount() int { return h.messageCount }

// OrderMessageCount são as mensagens que viraram item no carrinho. Junto com
// MessageCount é o sinal que separa a conta de instrução da compradora: a
// primeira fala muito E gera pedido.
func (h *SeenHandle) OrderMessageCount() int { return h.orderMessageCount }

func (h *SeenHandle) LastSeenAt() *time.Time { return h.lastSeenAt }

// Blocked reporta se existe bloqueio ATIVO para este arroba na loja.
func (h *SeenHandle) Blocked() bool { return h.blocked }
