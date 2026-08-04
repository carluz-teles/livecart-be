package live

// =============================================================================
// [IGTRACE] — instrumentação TEMPORÁRIA da ingestão de comentário
//
// Existe para uma pergunta: por que a Meta para de entregar live_comments no
// meio da transmissão, e o que o sistema decide com o que chega depois disso.
//
// TODO REMOVER quando a causa for identificada.
// `grep -rn "\[IGTRACE\]" apps/api` acha tudo desta investigação.
// =============================================================================

// TracePrefix marca toda linha desta investigação.
const TracePrefix = "[IGTRACE] "

// commentOrigin diz de onde o comentário veio.
//
// É a distinção que o log não tinha e que muda tudo na leitura: webhook e
// polling geram linhas idênticas a partir daqui, e só o webhook produz um
// comment_id que a Meta aceita no private reply. Um pedido "sem DM" tem causas
// diferentes conforme a origem, e sem este campo os dois casos eram o mesmo.
//
// O discriminador é o AccountID, que só o webhook traz em entry.id — o polling
// agora o preenche a partir da integração, então usamos o RawPayload, que é
// exclusivo do webhook e não é reconstruível.
func commentOrigin(input ProcessInstagramCommentInput) string {
	if len(input.RawPayload) > 0 {
		return "webhook"
	}
	return "polling"
}
