package notification

// A fila de espera tem DOIS avisos, e só um é entregável.
//
//	waitlist_joined    — o comprador pede item esgotado. Sai no mesmo instante,
//	                     por private reply ao comentário, que é a permissão que
//	                     o próprio comentário concede. FUNCIONA.
//
//	waitlist_notified  — o estoque volta e ela é promovida. Acontece depois,
//	                     desacoplado de qualquer comentário novo, então sobrava
//	                     o DM — e o Instagram só abre a janela de 24h quando o
//	                     COMPRADOR escreve para a conta. Foram 3 tentativas e 3
//	                     recusas 403 na live de 16/08. Removido.
//
// Os nomes são quase iguais e o risco é apagar o errado: sem waitlist_joined a
// compradora pede um item esgotado e não recebe nada — nem o aviso de que entrou
// na fila, nem explicação. Ela acha que o pedido não foi registrado.

import "testing"

func TestAvisoDeEntradaNaFilaContinuaExistindo(t *testing.T) {
	d := DefaultSettings()

	section := templateSection(&d, TypeWaitlistJoined)
	if section == nil {
		t.Fatal("waitlist_joined sumiu — é o único aviso de fila que o Instagram " +
			"entrega, e sem ele quem pede item esgotado fica sem resposta")
	}
	if section.Template == "" {
		t.Error("waitlist_joined ficou sem texto padrão")
	}
}

// O gatilho removido não pode voltar pela porta dos fundos: reaparecer em
// CartFlowTypes sem envio faz a UI oferecer um campo que nunca entrega mensagem,
// que é exatamente o estado que motivou a remoção.
func TestPromocaoDaFilaNaoVoltaComoGatilhoConfiguravel(t *testing.T) {
	for _, tipo := range CartFlowTypes {
		if string(tipo) == "waitlist_notified" {
			t.Error("waitlist_notified voltou para CartFlowTypes — o Instagram bloqueia " +
				"esse DM, então o lojista configuraria uma mensagem que nunca sai")
		}
	}
}

// A tabela dmSections é o que liga uma chave aos três lados (Request, Settings,
// Response). Uma chave em CartFlowTypes sem entrada aqui é chave morta — o comentário
// dela conta que foi assim que waitlist_notified existiu por meses sem UI.
func TestTodoGatilhoDeCartFlowTypesTemEntradaNaTabela(t *testing.T) {
	for _, tipo := range CartFlowTypes {
		encontrado := false
		for i := range dmSections {
			if dmSections[i].Type == tipo {
				encontrado = true
				break
			}
		}
		// Os gatilhos de e-mail não passam por dmSections (que é só DM).
		if !encontrado && templateSection(&Settings{}, tipo) != nil {
			t.Errorf("%s está em CartFlowTypes mas não em dmSections — nenhuma UI lê nem edita", tipo)
		}
	}
}
