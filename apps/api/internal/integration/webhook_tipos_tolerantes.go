package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// textoOuNumero aceita o mesmo campo vindo como STRING ou como NÚMERO.
//
// O Tiny grafa o mesmo campo das duas formas conforme o evento. Em 02/09/2026
// o `numero` do pedido chegou como número e o struct esperava string: o
// Unmarshal INTEIRO caiu, e como o handler seguia adiante com o struct zerado,
// o `idPedido` sumia junto. Nove notas fiscais foram descartadas naquele dia
// com "missing idPedido" — o id estava no corpo o tempo todo.
//
// Um campo lido só para identificação não pode derrubar o payload dos outros.
// Este tipo é a promessa de que ele não derruba.
type textoOuNumero string

func (t *textoOuNumero) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*t = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*t = textoOuNumero(s)
		return nil
	}
	// Número cru: guarda os dígitos COMO VIERAM, sem passar por float64 — este
	// campo é identificador, e float arredonda identificador grande.
	*t = textoOuNumero(b)
	return nil
}

func (t textoOuNumero) String() string { return string(t) }

// limitarPayload corta o corpo para caber numa linha de log.
//
// O payload só aparece quando o parse FALHA, e é a única pista de qual campo
// mudou de forma do outro lado. Sem ele, o log diz que algo quebrou e não diz o
// quê — foi o que fez as nove notas de 02/09 levarem um dia para serem notadas.
func limitarPayload(b []byte) string {
	const teto = 600
	if len(b) <= teto {
		return string(b)
	}
	return fmt.Sprintf("%s… (%d bytes)", b[:teto], len(b))
}
