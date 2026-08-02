package notification

// RN-28 — os cinco gatilhos. O que este arquivo guarda NÃO é "o texto padrão é
// esse": é a cadeia inteira de uma chave de template. Uma chave nova precisa
// atravessar cinco lugares (tipo, Settings, default, DTO do HTTP, escopo de
// variáveis) e o custo de esquecer UM deles é a chave existir e nunca funcionar
// — foi exatamente o que aconteceu com waitlist_notified, que viveu meses no
// domínio sem existir no HTTP, inconfigurável e sem ninguém perceber.

import (
	"strings"
	"testing"
)

// TestTodoGatilhoAtravessaAsCincoCamadas é o guard da classe de bug acima.
func TestTodoGatilhoAtravessaAsCincoCamadas(t *testing.T) {
	defaults := DefaultSettings()

	for _, notifType := range CartFlowTypes {
		t.Run(string(notifType), func(t *testing.T) {
			// 1. Tem seção em Settings e default preenchido — sem isso a loja
			// que nunca customizou não recebe nada.
			section := templateSection(&defaults, notifType)
			if section == nil {
				t.Fatalf("%s não tem seção em DefaultSettings — o gatilho nasce morto para toda loja existente", notifType)
			}
			if strings.TrimSpace(section.Template) == "" {
				t.Errorf("%s tem template padrão vazio", notifType)
			}

			// 2. Está no DTO do HTTP nas TRÊS direções (request → settings →
			// response). É esta a camada que faltava no waitlist_notified.
			var found *dmSection
			for i := range dmSections {
				if dmSections[i].Type == notifType {
					found = &dmSections[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("%s não está em dmSections — nenhuma UI consegue ler nem editar este gatilho", notifType)
			}

			req := &UpdateSettingsRequest{}
			found.set(&Settings{}, nil) // apenas garante que o setter não entra em pânico
			if got := found.fromReq(req); got != nil {
				t.Errorf("%s: fromReq de um request vazio devolveu não-nil — o merge parcial depende do nil", notifType)
			}

			// 3. Tem escopo de variáveis: o editor precisa saber o que oferecer.
			if vars := VariablesForTemplate(string(notifType)); len(vars) == 0 {
				t.Errorf("%s não tem escopo em templateVariableScopes", notifType)
			}

			// 4. O template padrão cabe no limite do Instagram já renderizado.
			if _, err := ValidateTemplate(section.Template, SampleVariables()); err != nil {
				t.Errorf("%s: template padrão inválido: %v", notifType, err)
			}
		})
	}
}

// TestTemplatePadraoSoUsaVariaveisDoProprioEscopo pega o erro mais provável ao
// escrever copy: o texto promete um dado que aquele momento não tem. Um
// {total} no gatilho de "comentou antes de começar" renderiza vazio na DM do
// comprador — não dá erro, só entrega uma frase quebrada.
func TestTemplatePadraoSoUsaVariaveisDoProprioEscopo(t *testing.T) {
	defaults := DefaultSettings()

	for _, notifType := range CartFlowTypes {
		section := templateSection(&defaults, notifType)
		if section == nil {
			continue
		}
		allowed := map[string]bool{}
		for _, v := range VariablesForTemplate(string(notifType)) {
			allowed[v.Name] = true
		}
		// {live_titulo} é alias depreciado de {evento} e não aparece em escopo
		// nenhum, mas continua renderizando.
		allowed["{live_titulo}"] = true

		for name := range variableCatalog {
			if strings.Contains(section.Template, name) && !allowed[name] {
				t.Errorf("%s usa %s no texto padrão, que não está no escopo desse gatilho — renderiza vazio para o comprador",
					notifType, name)
			}
		}
	}
}

// TestEventoEhAliasDeLiveTitulo trava a renomeação da RN-19. Trocar a chave sem
// alias renderizaria {live_titulo} cru na DM de todo lojista que já salvou um
// template com o nome antigo.
func TestEventoEhAliasDeLiveTitulo(t *testing.T) {
	vars := TemplateVariables{LiveTitulo: "Semana Black"}

	if got := RenderTemplate("na {evento}", vars); got != "na Semana Black" {
		t.Errorf("{evento} = %q", got)
	}
	if got := RenderTemplate("na {live_titulo}", vars); got != "na Semana Black" {
		t.Errorf("{live_titulo} deixou de renderizar: %q", got)
	}
}

// TestOsCincoGatilhosSaoDefaultOn: um gatilho que nasce desligado é um gatilho
// que não existe. Como o JSONB de toda loja da base antecede estas chaves, o
// default tem de valer quando a seção vem nil — senão a RN-28 entra no ar
// silenciosa em 100% das lojas.
func TestOsCincoGatilhosSaoDefaultOn(t *testing.T) {
	svc := &Service{}
	vazio := &Settings{} // loja cujo JSONB não conhece nenhuma chave nova

	novos := []NotificationType{
		TypeOutOfWindowScheduled,
		TypeOutOfWindowSessionEnded,
		TypeOutOfWindowEventEnded,
		TypeEventDeadlineStarted,
		TypeWaitlistUnfulfilled,
		TypeWaitlistNotified,
	}

	for _, notifType := range novos {
		got := svc.getTemplateSettings(vazio, notifType)
		if got == nil {
			t.Errorf("%s: seção nil não caiu no default — o gatilho nunca dispara", notifType)
			continue
		}
		if !got.Enabled {
			t.Errorf("%s: default desligado", notifType)
		}
	}
}

// TestMergePreservaOsGatilhosNovos estende o guard do merge parcial às chaves
// desta fatia: elas herdariam exatamente o mesmo apagamento silencioso.
func TestMergePreservaOsGatilhosNovos(t *testing.T) {
	tpl := func(text string) *TemplateSettings {
		return &TemplateSettings{Enabled: true, Template: text}
	}

	current := Settings{
		OutOfWindowScheduled:    tpl("agendado configurado"),
		OutOfWindowSessionEnded: tpl("sessao configurada"),
		OutOfWindowEventEnded:   tpl("evento configurado"),
		EventDeadlineStarted:    tpl("prazo configurado"),
		WaitlistUnfulfilled:     tpl("fila configurada"),
	}

	// O save de um card qualquer (o FE manda só o que o lojista editou).
	got := mergeSettings(current, Settings{CheckoutImmediate: tpl("novo")})

	for name, section := range map[string]*TemplateSettings{
		"out_of_window_scheduled":     got.OutOfWindowScheduled,
		"out_of_window_session_ended": got.OutOfWindowSessionEnded,
		"out_of_window_event_ended":   got.OutOfWindowEventEnded,
		"event_deadline_started":      got.EventDeadlineStarted,
		"waitlist_unfulfilled":        got.WaitlistUnfulfilled,
	} {
		if section == nil {
			t.Errorf("%s foi apagado pelo save de outro card", name)
		}
	}
}

// TestRespostaTrazTodosOsCards: se a chave não sai no GET, o card não existe na
// aba de Comunicações — e o lojista não consegue nem ver o texto que os
// compradores dele estão recebendo.
func TestRespostaTrazTodosOsCards(t *testing.T) {
	// Loja que nunca customizou nada: a resposta ainda tem de trazer os
	// defaults, senão o editor abre vazio e o primeiro save grava um template
	// em branco por cima do que de fato é enviado.
	resp := toSettingsResponse(&Settings{})

	cards := map[string]*TemplateSettingsResponse{
		"checkout_immediate":          resp.CheckoutImmediate,
		"item_added":                  resp.ItemAdded,
		"checkout_reminder":           resp.CheckoutReminder,
		"waitlist_notified":           resp.WaitlistNotified,
		"out_of_window_scheduled":     resp.OutOfWindowScheduled,
		"out_of_window_session_ended": resp.OutOfWindowSessionEnded,
		"out_of_window_event_ended":   resp.OutOfWindowEventEnded,
		"event_deadline_started":      resp.EventDeadlineStarted,
		"waitlist_unfulfilled":        resp.WaitlistUnfulfilled,
	}
	for name, card := range cards {
		if card == nil {
			t.Errorf("%s não aparece na resposta do GET de settings", name)
			continue
		}
		if strings.TrimSpace(card.Template) == "" {
			t.Errorf("%s veio com template vazio", name)
		}
	}
}
