package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Client publishes canonical events onto the asynq queue. It is used by the
// outbox relay (Fase 03), not directly by domain services — services emit via
// the transactional outbox so an event is never lost between the state change
// commit and the enqueue.
type Client struct {
	inner *asynq.Client
	// inspector existe só para Reschedule: mover um agendamento ETA exige
	// APAGAR a task pendente antes de re-enfileirar (ver Reschedule).
	inspector *asynq.Inspector
	// logger registra o enfileiramento dos scheduled-commands ETA (Schedule),
	// que NÃO passam pelo outbox/relay — sem isto eles entram na fila sem rastro.
	// Os eventos de domínio (via outbox) são logados pelo Relay no enqueue.
	logger *zap.Logger
}

// NewClient builds an event publisher backed by the given Redis connection.
// Callers pass a RedisConnOpt (not a bare addr) so managed Redis with a
// password/TLS — e.g. Railway — works, not just local no-auth Redis.
func NewClient(redisOpt asynq.RedisConnOpt, logger *zap.Logger) *Client {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Client{
		inner:     asynq.NewClient(redisOpt),
		inspector: asynq.NewInspector(redisOpt),
		logger:    logger.Named("events-client"),
	}
}

// Enqueue serializes the envelope as an asynq task and enqueues it. The task
// type is the canonical event name; the whole envelope (source, metadata, trace
// context, payload) is the task payload so the consumer can reconstruct it.
//
// The queue and its default retry/timeout policy come from the envelope unless
// the caller overrides them via opts.
func (c *Client) Enqueue(ctx context.Context, env Envelope, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	// Inject the producer's trace context so the consumer continues the span.
	if env.TraceID == "" {
		if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
			env.TraceID = sc.TraceID().String()
			env.SpanID = sc.SpanID().String()
		}
	}

	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshaling event envelope: %w", err)
	}

	queue := env.Queue()
	baseOpts := []asynq.Option{asynq.Queue(queue)}
	if p, ok := DefaultPolicies[queue]; ok {
		baseOpts = append(baseOpts, asynq.MaxRetry(p.MaxRetry), asynq.Timeout(p.Timeout))
	}
	// Use the event id as the task id so a duplicated relay enqueue is a no-op
	// on the queue itself (belt-and-suspenders with the outbox published flag).
	if env.EventID != "" {
		baseOpts = append(baseOpts, asynq.TaskID(env.EventID))
	}

	task := asynq.NewTask(string(env.Name), data, append(baseOpts, opts...)...)
	info, err := c.inner.EnqueueContext(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("enqueuing event %s: %w", env.Name, err)
	}
	return info, nil
}

// Schedule enqueues an internal command task to run at processAt. asynq stores
// it in a per-queue scheduled sorted set (scored by the process-at time) and its
// scheduler forwards it to the ready queue when due — precise ETA execution, not
// the lossy Redis key-TTL / keyspace-notification pattern.
//
// taskID makes scheduling idempotent: a duplicate enqueue while a task with the
// same id is still pending returns ErrTaskIDConflict, which we swallow ("already
// armed"). A task that already ran and completed frees its id, so re-arming
// (self-reschedule on window extension) works.
func (c *Client) Schedule(ctx context.Context, processAt time.Time, name Name, taskID string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling %s command payload: %w", name, err)
	}
	env := Envelope{Name: name, Source: SourceInternal, SchemaVersion: 1, Payload: body}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		env.TraceID = sc.TraceID().String()
		env.SpanID = sc.SpanID().String()
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling %s command envelope: %w", name, err)
	}
	_, err = c.inner.EnqueueContext(ctx, asynq.NewTask(string(name), data),
		asynq.ProcessAt(processAt.UTC()),
		asynq.TaskID(taskID),
		asynq.Queue(QueueNormal),
		asynq.MaxRetry(5),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		return nil // already scheduled — the pending task will handle it
	}
	if err != nil {
		return fmt.Errorf("scheduling %s at %s: %w", name, processAt.UTC().Format(time.RFC3339), err)
	}
	// Scheduled-commands (cart.expire, session.publish, event.window_close, ...)
	// não passam pelo Relay, então este é o único ponto onde a task "nasce" na
	// fila. Correlaciona com o "event observed" do consumidor via trace_id.
	c.logger.Info("command scheduled",
		zap.String("event", string(name)),
		zap.String("task_id", taskID),
		zap.String("queue", QueueNormal),
		zap.Time("process_at", processAt.UTC()),
		zap.String("source", string(SourceInternal)),
		zap.String("trace_id", env.TraceID),
	)
	return nil
}

// Reschedule MOVE um agendamento ETA já armado para um novo horário.
//
// Schedule sozinho não faz isso, e essa era a raiz de três bugs: o asynq guarda
// uma chave por TaskID enquanto a task existe (scheduled OU active), e um
// enqueue com id repetido devolve ErrTaskIDConflict — que Schedule engole como
// "já agendado". Resultado: antecipar o fim do evento fechava na hora antiga
// (CA-05.4) e estender o prazo do carrinho pela fila (RN-10) também não movia
// nada. Não era só "não dá para encurtar": não dava para mover em direção
// nenhuma.
//
// Apagar antes de re-enfileirar resolve para a task PENDENTE, que é o caso de
// todo re-agendamento vindo de fora (edição do evento, promoção da fila).
//
// LIMITE CONHECIDO: o asynq recusa apagar uma task em estado ACTIVE, então um
// handler que tente re-agendar a si mesmo continua caindo no conflito. Os dois
// handlers que fazem isso (RunScheduledExpiry, RunScheduledEventClose) são
// guard-first e nunca executam a ação fora de hora, e o sweep de eventos é a
// rede — mas o re-arm de dentro do handler segue sendo best-effort.
func (c *Client) Reschedule(ctx context.Context, processAt time.Time, name Name, taskID string, payload any) error {
	if err := c.inspector.DeleteTask(QueueNormal, taskID); err != nil {
		// "não existe" é o caso normal: primeira vez, ou a task já rodou.
		if !errors.Is(err, asynq.ErrTaskNotFound) && !errors.Is(err, asynq.ErrQueueNotFound) {
			return fmt.Errorf("rescheduling %s (%s): deleting pending task: %w", name, taskID, err)
		}
	}
	return c.Schedule(ctx, processAt, name, taskID, payload)
}

// Cancel APAGA um agendamento ETA ainda pendente.
//
// Existe porque cancelar não é reagendar: Reschedule sempre re-enfileira, e não
// havia como simplesmente desistir de uma task já armada. "Não existe" é
// desfecho normal (a task já rodou, ou nunca chegou a ser armada), então não
// vira erro.
//
// MESMO LIMITE do Reschedule: o asynq recusa apagar uma task em estado ACTIVE.
// Por isso o cancelamento de verdade tem de ser o guard no banco — quem
// cancela move a linha para fora de 'scheduled', e a task que dispare mesmo
// assim não encontra nada para reivindicar. Esta chamada é a limpeza, não a
// garantia.
func (c *Client) Cancel(ctx context.Context, taskID string) error {
	err := c.inspector.DeleteTask(QueueNormal, taskID)
	if err == nil || errors.Is(err, asynq.ErrTaskNotFound) || errors.Is(err, asynq.ErrQueueNotFound) {
		return nil
	}
	return fmt.Errorf("cancelling scheduled task %s: %w", taskID, err)
}

// Close releases the underlying Redis connection.
func (c *Client) Close() error {
	if err := c.inspector.Close(); err != nil {
		_ = c.inner.Close()
		return err
	}
	return c.inner.Close()
}
