package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/UNagent-1D/conversation-chat/internal/domain"
	"github.com/redis/go-redis/v9"
)

const (
	keyCtx      = "ctx:%s"
	keyHist     = "hist:%s"
	keyState    = "state:%s"
	keyOpQueue  = "op_queue:%s"
	keyEscTTL   = "esc_ttl:%s"
	keyEvents   = "events:stats"
)

// RedisRepo handles all Redis operations for session state management.
type RedisRepo struct {
	client *redis.Client
}

// NewRedisRepo creates a new RedisRepo.
func NewRedisRepo(client *redis.Client) *RedisRepo {
	return &RedisRepo{client: client}
}

// --- ContextEnvelope ---

// SetContext caches the ContextEnvelope for a session.
func (r *RedisRepo) SetContext(ctx context.Context, sessionID string, env domain.ContextEnvelope, ttl time.Duration) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal context envelope: %w", err)
	}
	return r.client.Set(ctx, fmt.Sprintf(keyCtx, sessionID), data, ttl).Err()
}

// GetContext retrieves the cached ContextEnvelope for a session.
func (r *RedisRepo) GetContext(ctx context.Context, sessionID string) (*domain.ContextEnvelope, error) {
	data, err := r.client.Get(ctx, fmt.Sprintf(keyCtx, sessionID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get context envelope: %w", err)
	}
	var env domain.ContextEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("unmarshal context envelope: %w", err)
	}
	return &env, nil
}

// RefreshContextTTL resets the TTL on the context key (called on each turn).
func (r *RedisRepo) RefreshContextTTL(ctx context.Context, sessionID string, ttl time.Duration) error {
	return r.client.Expire(ctx, fmt.Sprintf(keyCtx, sessionID), ttl).Err()
}

// --- History ---

// AppendTurn appends a turn to the history list and refreshes the list TTL.
func (r *RedisRepo) AppendTurn(ctx context.Context, sessionID string, turn domain.Turn, ttl time.Duration) error {
	data, err := json.Marshal(turn)
	if err != nil {
		return fmt.Errorf("marshal turn: %w", err)
	}
	pipe := r.client.Pipeline()
	pipe.RPush(ctx, fmt.Sprintf(keyHist, sessionID), data)
	pipe.Expire(ctx, fmt.Sprintf(keyHist, sessionID), ttl)
	_, err = pipe.Exec(ctx)
	return err
}

// GetHistory retrieves all turns from the history list.
func (r *RedisRepo) GetHistory(ctx context.Context, sessionID string) ([]domain.Turn, error) {
	raw, err := r.client.LRange(ctx, fmt.Sprintf(keyHist, sessionID), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("get history: %w", err)
	}
	turns := make([]domain.Turn, 0, len(raw))
	for _, s := range raw {
		var t domain.Turn
		if err := json.Unmarshal([]byte(s), &t); err != nil {
			return nil, fmt.Errorf("unmarshal turn: %w", err)
		}
		turns = append(turns, t)
	}
	return turns, nil
}

// --- Session State ---

// SetState sets the session state string.
func (r *RedisRepo) SetState(ctx context.Context, sessionID string, state domain.SessionState, ttl time.Duration) error {
	return r.client.Set(ctx, fmt.Sprintf(keyState, sessionID), string(state), ttl).Err()
}

// GetState retrieves the current session state.
func (r *RedisRepo) GetState(ctx context.Context, sessionID string) (domain.SessionState, error) {
	val, err := r.client.Get(ctx, fmt.Sprintf(keyState, sessionID)).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get state: %w", err)
	}
	return domain.SessionState(val), nil
}

// --- Escalation ---

// AddToOpQueue adds a session to the operator queue sorted set (scored by Unix timestamp).
func (r *RedisRepo) AddToOpQueue(ctx context.Context, tenantID, sessionID string, at time.Time) error {
	return r.client.ZAdd(ctx, fmt.Sprintf(keyOpQueue, tenantID), redis.Z{
		Score:  float64(at.Unix()),
		Member: sessionID,
	}).Err()
}

// RemoveFromOpQueue removes a session from the operator queue.
func (r *RedisRepo) RemoveFromOpQueue(ctx context.Context, tenantID, sessionID string) error {
	return r.client.ZRem(ctx, fmt.Sprintf(keyOpQueue, tenantID), sessionID).Err()
}

// SetEscalationTTL sets a key that expires after operator_ttl_seconds.
// The Chat service checks for this key's existence on the next turn to detect TTL expiry.
func (r *RedisRepo) SetEscalationTTL(ctx context.Context, sessionID string, ttl time.Duration) error {
	return r.client.Set(ctx, fmt.Sprintf(keyEscTTL, sessionID), "1", ttl).Err()
}

// EscalationTTLActive returns true if the escalation TTL key still exists (operator window open).
func (r *RedisRepo) EscalationTTLActive(ctx context.Context, sessionID string) (bool, error) {
	n, err := r.client.Exists(ctx, fmt.Sprintf(keyEscTTL, sessionID)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- Stats Events ---

// EmitEvent publishes an event to the Redis Stream consumed by the Stats service.
func (r *RedisRepo) EmitEvent(ctx context.Context, eventType string, fields map[string]any) error {
	fields["event_type"] = eventType
	return r.client.XAdd(ctx, &redis.XAddArgs{
		Stream: keyEvents,
		Values: fields,
	}).Err()
}

// --- Cleanup ---

// DeleteSession removes all Redis keys for a closed session.
func (r *RedisRepo) DeleteSession(ctx context.Context, sessionID string) error {
	keys := []string{
		fmt.Sprintf(keyCtx, sessionID),
		fmt.Sprintf(keyHist, sessionID),
		fmt.Sprintf(keyState, sessionID),
		fmt.Sprintf(keyEscTTL, sessionID),
	}
	return r.client.Del(ctx, keys...).Err()
}

// Ping checks Redis connectivity (used by health handler).
func (r *RedisRepo) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}
