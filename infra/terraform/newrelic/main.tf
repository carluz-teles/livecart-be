# ==============================================================================
# LiveCart — New Relic Dashboard (Fatia 5/6 do projeto de telemetria)
# ==============================================================================
#
# Fonte de dados: 5 custom events emitidos pelo exporter
# (apps/api/internal/telemetry/exporter/payloads.go), conta New Relic 8291202:
#   - LiveCommerceEvent      (action=created|ended)
#   - LiveCommerceCart       (status=created|checkout_armed|paid|expired|cancelled|refunded)
#   - LiveCommerceCartItem   (1 por item vendido)
#   - LiveCommercePayment    (outcome=approved|rejected|recorded|refunded)
#   - LiveCommerceComment
#
# ARMADILHA DE GMV (ver payloads.go, comentário de LiveCommercePaymentPayload):
# um cart pago emite DOIS eventos LiveCommercePayment para o mesmo cart_id —
# outcome="approved" (cart.paid, sem fee/plan) e outcome="recorded" (gmv.recorded,
# com fee_cents/plan/billable). Somar amount_cents por cart_id através dos dois
# outcomes DOBRA o GMV.
#
# Decisão deste dashboard: todo widget de GMV usa `sum(gmv_cents) FROM
# LiveCommerceCart WHERE status = 'paid'` — é a fonte mais direta (1 evento por
# cart pago) e não tem esse risco de duplicidade. LiveCommercePayment só é usado
# aqui para Payment Success Rate (contagem de outcomes approved/rejected, não
# soma de amount_cents), nunca para GMV.
#
# Retenção de dados: controlada no nível de conta/plano do New Relic para custom
# events, não por Terraform de dashboard. Ver var.data_retention_days
# (documentação, não um recurso real) em variables.tf.
# ==============================================================================

resource "newrelic_one_dashboard" "live_commerce" {
  name        = "${var.project_name}-live-commerce"
  description = "LiveCart — visão macro (todos os eventos, filtrável por ambiente) e micro (por live_event_id) do funil de live commerce: eventos/sessões, carrinhos, pagamentos, produtos e comentários. Staging e produção compartilham esta conta/dashboard — use a variável 'environment' para separar."
  permissions = var.dashboard_permissions

  # ----------------------------------------------------------------------
  # Page 1 — Macro: agregado de todos os eventos, filtrado pela dashboard
  # variable `environment` (staging/production compartilham conta+dashboard —
  # ver variable "environment" abaixo). A variable tem default_values fixo, o
  # que garante que {{environment}} nunca chega vazio às queries abaixo.
  # ----------------------------------------------------------------------
  page {
    name        = "Macro"
    description = "Visão agregada de todos os live events de um ambiente (selecione em 'environment'), sem filtro por evento."

    widget_billboard {
      title  = "Active Events (24h)"
      row    = 1
      column = 1
      width  = 3
      height = 3

      nrql_query {
        query = "SELECT uniqueCount(live_event_id) AS 'Active Events' FROM LiveCommerceEvent WHERE action = 'created' AND environment = {{environment}} AND live_event_id NOT IN (SELECT uniques(live_event_id) FROM LiveCommerceEvent WHERE action = 'ended' AND environment = {{environment}} SINCE 24 hours ago) SINCE 24 hours ago"
      }
    }

    widget_billboard {
      title  = "GMV total (R$)"
      row    = 1
      column = 4
      width  = 3
      height = 3

      nrql_query {
        query = "SELECT sum(gmv_cents) / 100 AS 'GMV (R$)' FROM LiveCommerceCart WHERE status = 'paid' AND environment = {{environment}} SINCE 1 day ago"
      }
    }

    widget_billboard {
      title  = "Conversion Rate geral (%)"
      row    = 1
      column = 7
      width  = 3
      height = 3

      nrql_query {
        query = "SELECT filter(count(*), WHERE status = 'paid') / filter(count(*), WHERE status = 'checkout_armed') * 100 AS 'Conversion %' FROM LiveCommerceCart WHERE environment = {{environment}} SINCE 1 day ago"
      }
    }

    widget_billboard {
      title  = "Payment Success Rate (%)"
      row    = 1
      column = 10
      width  = 3
      height = 3

      nrql_query {
        query = "SELECT filter(count(*), WHERE outcome = 'approved') / filter(count(*), WHERE outcome IN ('approved', 'rejected')) * 100 AS 'Payment Success %' FROM LiveCommercePayment WHERE environment = {{environment}} SINCE 1 day ago"
      }
    }

    widget_line {
      title  = "GMV acumulado (R$)"
      row    = 4
      column = 1
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT sum(gmv_cents) / 100 AS 'GMV (R$)' FROM LiveCommerceCart WHERE status = 'paid' AND environment = {{environment}} TIMESERIES SINCE 1 day ago"
      }
    }

    widget_line {
      title  = "Cart funnel (created -> checkout_armed -> paid)"
      row    = 4
      column = 7
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT filter(count(*), WHERE status = 'created') AS 'Created', filter(count(*), WHERE status = 'checkout_armed') AS 'Checkout Armed', filter(count(*), WHERE status = 'paid') AS 'Paid' FROM LiveCommerceCart WHERE environment = {{environment}} TIMESERIES SINCE 1 day ago"
      }
    }

    widget_table {
      title  = "Top eventos por engajamento (comentários)"
      row    = 7
      column = 1
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT count(*) AS 'Comentários' FROM LiveCommerceComment WHERE environment = {{environment}} FACET live_event_id SINCE 1 day ago LIMIT 10"
      }
    }

    widget_table {
      title  = "Top produtos globais por revenue"
      row    = 7
      column = 7
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT sum(revenue_cents) / 100 AS 'Revenue (R$)' FROM LiveCommerceCartItem WHERE environment = {{environment}} FACET product_name SINCE 1 day ago LIMIT 10"
      }
    }

    widget_billboard {
      title  = "Checkout duration (ms) — p50/p95/p99"
      row    = 10
      column = 1
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT percentile(checkout_duration_ms, 50, 95, 99) FROM LiveCommerceCart WHERE status = 'paid' AND environment = {{environment}} SINCE 1 day ago"
      }
    }

    widget_markdown {
      title  = "Gap conhecido — Erros"
      row    = 10
      column = 7
      width  = 6
      height = 3

      text = "### Erros\n\nNão existe hoje um `eventType` de erro/log exportado para o New Relic (o exporter só emite os 5 custom events LiveCommerce*). Este espaço fica reservado para quando essa telemetria existir — ver Fatia 6 (log drain), fora do escopo desta fatia."
    }
  }

  # ----------------------------------------------------------------------
  # Page 2 — Micro: filtrado por live_event_id via dashboard variable
  # ----------------------------------------------------------------------
  page {
    name        = "Micro (por evento)"
    description = "Drill-down de um live_event_id específico — selecione o evento na variável do dashboard."

    widget_billboard {
      title  = "Evento rolando agora? (created sem ended, 24h)"
      row    = 1
      column = 1
      width  = 4
      height = 3

      nrql_query {
        query = "SELECT uniqueCount(live_event_id) AS 'Rolando' FROM LiveCommerceEvent WHERE action = 'created' AND live_event_id = {{live_event_id}} AND live_event_id NOT IN (SELECT uniques(live_event_id) FROM LiveCommerceEvent WHERE action = 'ended' AND live_event_id = {{live_event_id}} SINCE 24 hours ago) SINCE 24 hours ago"
      }
    }

    widget_billboard {
      title  = "GMV do evento (R$)"
      row    = 1
      column = 5
      width  = 4
      height = 3

      nrql_query {
        query = "SELECT sum(gmv_cents) / 100 AS 'GMV (R$)' FROM LiveCommerceCart WHERE status = 'paid' AND live_event_id = {{live_event_id}} SINCE 1 day ago"
      }
    }

    widget_billboard {
      title  = "Comment -> Cart (%)"
      row    = 1
      column = 9
      width  = 4
      height = 3

      nrql_query {
        query = "SELECT filter(uniqueCount(cart_id), WHERE converted_to_cart) / count(*) * 100 AS 'Comment->Cart %' FROM LiveCommerceComment WHERE live_event_id = {{live_event_id}} SINCE 1 day ago"
      }
    }

    widget_funnel {
      title  = "Funil comentário -> carrinho -> pago"
      row    = 4
      column = 1
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT filter(count(*), WHERE eventType() = 'LiveCommerceComment') AS 'Comentários', filter(uniqueCount(cart_id), WHERE eventType() = 'LiveCommerceComment' AND converted_to_cart) AS 'Convertidos p/ Carrinho', filter(uniqueCount(cart_id), WHERE eventType() = 'LiveCommerceCart' AND status = 'paid') AS 'Pagos' FROM LiveCommerceComment, LiveCommerceCart WHERE live_event_id = {{live_event_id}} SINCE 1 day ago"
      }
    }

    widget_bar {
      title  = "GMV por produto (R$)"
      row    = 4
      column = 7
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT sum(revenue_cents) / 100 AS 'Revenue (R$)' FROM LiveCommerceCartItem WHERE live_event_id = {{live_event_id}} FACET product_name SINCE 1 day ago LIMIT 20"
      }
    }

    widget_bar {
      title  = "Sessões por plataforma"
      row    = 7
      column = 1
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT uniqueCount(session_id) AS 'Sessões' FROM LiveCommerceEvent WHERE live_event_id = {{live_event_id}} FACET platform SINCE 1 day ago"
      }
    }

    widget_billboard {
      title  = "Checkout duration do evento (ms) — p50/p95/p99"
      row    = 7
      column = 7
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT percentile(checkout_duration_ms, 50, 95, 99) FROM LiveCommerceCart WHERE status = 'paid' AND live_event_id = {{live_event_id}} SINCE 1 day ago"
      }
    }

    widget_table {
      title  = "Conversion Rate do evento (%)"
      row    = 10
      column = 1
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT filter(count(*), WHERE status = 'paid') / filter(count(*), WHERE status = 'checkout_armed') * 100 AS 'Conversion %' FROM LiveCommerceCart WHERE live_event_id = {{live_event_id}} SINCE 1 day ago"
      }
    }

    widget_table {
      title  = "Timeline do evento (últimos 60min)"
      row    = 10
      column = 7
      width  = 6
      height = 3

      nrql_query {
        query = "SELECT * FROM LiveCommerceCart, LiveCommercePayment, LiveCommerceComment WHERE live_event_id = {{live_event_id}} SINCE 1 hour ago LIMIT 100"
      }
    }

  }

  # live_event_id is only referenced by widgets on the "Micro" page, but the
  # provider schema requires dashboard-local variables at the resource level
  # (a sibling of page blocks), not nested inside a specific page.
  variable {
    name                 = "live_event_id"
    title                = "Live Event"
    type                 = "string"
    replacement_strategy = "string"
    is_multi_selection   = false
  }

  # environment filters the Macro page (staging vs production share this
  # single account+dashboard, per apps/api/internal/telemetry/exporter's
  # `environment` attribute on every LiveCommerce* custom event). `enum`
  # (not free-text "string") constrains the operator to real values, and
  # default_values guarantees {{environment}} is never empty/unselected —
  # an empty NRQL variable would make `WHERE environment = {{environment}}`
  # either error or silently match nothing, which is why this is not left
  # to a free-text default. Not referenced on the "Micro" page: live_event_id
  # already isolates a single event unambiguously, and events only ever
  # belong to one environment, so stacking both filters there would be
  # redundant.
  variable {
    name                 = "environment"
    title                = "Environment"
    type                 = "enum"
    replacement_strategy = "string"
    is_multi_selection   = false
    default_values       = [var.environment]

    item {
      title = "staging"
      value = "staging"
    }

    item {
      title = "production"
      value = "production"
    }
  }
}
