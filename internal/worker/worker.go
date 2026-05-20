package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/UNagent-1D/conversation-chat/internal/channel"
	"github.com/UNagent-1D/conversation-chat/internal/service"
)

const (
	queueRequests = "chat_requests"
	queueResults  = "chat_results"
	// RabbitMQ can take a while to accept connections after its container
	// reports healthy; retry for ~60s before giving up so the worker
	// survives a cold start of the stack.
	maxRetries = 30
	retryDelay = 2 * time.Second
)

// ChatJob is the message consumed from chat_requests.
type ChatJob struct {
	JobID     string `json:"job_id"`
	ChatID    int64  `json:"chat_id"`
	TenantID  string `json:"tenant_id"`
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// ChatResult is the message published to chat_results.
type ChatResult struct {
	JobID     string `json:"job_id"`
	ChatID    int64  `json:"chat_id"`
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

// Worker consumes jobs from chat_requests, calls ChatService.ProcessTurn,
// and publishes results to chat_results.
type Worker struct {
	chatSvc     *service.ChatService
	rabbitmqURL string
	logger      *slog.Logger
}

// New creates a Worker. chatSvc is the existing service instance from main.
func New(chatSvc *service.ChatService, rabbitmqURL string, logger *slog.Logger) *Worker {
	return &Worker{chatSvc: chatSvc, rabbitmqURL: rabbitmqURL, logger: logger}
}

// Start connects to RabbitMQ (with retries) and begins consuming.
// Blocks until ctx is cancelled or a fatal error occurs.
func (w *Worker) Start(ctx context.Context) error {
	conn, ch, err := w.connect()
	if err != nil {
		return fmt.Errorf("worker connect: %w", err)
	}
	defer conn.Close()
	defer ch.Close()

	msgs, err := ch.Consume(queueRequests, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("worker consume: %w", err)
	}

	w.logger.Info("worker: consuming chat_requests queue")

	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-msgs:
			if !ok {
				return fmt.Errorf("worker: channel closed unexpectedly")
			}
			w.handleDelivery(ctx, ch, d)
		}
	}
}

func (w *Worker) connect() (*amqp.Connection, *amqp.Channel, error) {
	var conn *amqp.Connection
	var err error

	for i := range maxRetries {
		conn, err = amqp.Dial(w.rabbitmqURL)
		if err == nil {
			break
		}
		w.logger.Warn("worker: RabbitMQ not ready, retrying",
			slog.Int("attempt", i+1),
			slog.String("error", err.Error()),
		)
		time.Sleep(retryDelay)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("dial rabbitmq after %d attempts: %w", maxRetries, err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("open channel: %w", err)
	}

	for _, q := range []string{queueRequests, queueResults} {
		if _, err := ch.QueueDeclare(q, true, false, false, false, nil); err != nil {
			ch.Close()
			conn.Close()
			return nil, nil, fmt.Errorf("declare queue %s: %w", q, err)
		}
	}

	return conn, ch, nil
}

func (w *Worker) handleDelivery(ctx context.Context, ch *amqp.Channel, d amqp.Delivery) {
	// Decrypt the AMQP body if agent-runtime sealed it. OpenBytes is a
	// passthrough when the content-type is plaintext, so this stays
	// compatible during the rollout window when BACKEND_CHANNEL_ENABLED
	// might be false on either side.
	plain, err := channel.OpenBytes(d.ContentType, d.Body)
	if err != nil {
		w.logger.Error("worker: failed to decrypt job envelope",
			slog.String("error", err.Error()),
			slog.String("content_type", d.ContentType),
		)
		_ = d.Nack(false, false)
		return
	}

	var job ChatJob
	if err := json.Unmarshal(plain, &job); err != nil {
		w.logger.Error("worker: failed to parse job", slog.String("error", err.Error()))
		_ = d.Nack(false, false)
		return
	}

	w.logger.Info("worker: processing job",
		slog.String("job_id", job.JobID),
		slog.String("session_id", job.SessionID),
	)

	resp, err := w.chatSvc.ProcessTurn(ctx, job.SessionID, service.TurnRequest{
		UserMessage: job.Message,
		MessageID:   job.JobID,
		ChannelKey:  "",
	})

	var resultText string
	if err != nil {
		w.logger.Error("worker: ProcessTurn error",
			slog.String("job_id", job.JobID),
			slog.String("error", err.Error()),
		)
		// Publish a real, user-facing message instead of an empty string so
		// the Telegram side never has to send a bare placeholder.
		resultText = "Lo siento, tuve un problema procesando tu mensaje. Por favor intenta de nuevo."
	} else {
		resultText = resp.Message.Text
	}

	result := ChatResult{
		JobID:     job.JobID,
		ChatID:    job.ChatID,
		SessionID: job.SessionID,
		Text:      resultText,
	}

	// Seal the result envelope so the agent-runtime consumer sees ciphertext
	// in the chat_results queue. channel.SealJSON falls back to plaintext +
	// application/json when the channel is disabled, keeping the contract
	// during gradual rollout.
	body, ct, _, err := channel.SealJSON(result)
	if err != nil {
		w.logger.Error("worker: failed to seal result envelope",
			slog.String("job_id", job.JobID),
			slog.String("error", err.Error()),
		)
		_ = d.Nack(false, false)
		return
	}
	pub := amqp.Publishing{
		ContentType:  ct,
		DeliveryMode: amqp.Persistent,
		Body:         body,
	}
	if pubErr := ch.PublishWithContext(ctx, "", queueResults, false, false, pub); pubErr != nil {
		w.logger.Error("worker: failed to publish result",
			slog.String("job_id", job.JobID),
			slog.String("error", pubErr.Error()),
		)
		_ = d.Nack(false, false)
		return
	}

	_ = d.Ack(false)
}
