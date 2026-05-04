package llm

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/UNagent-1D/conversation-chat/internal/apperrors"
)

// cbState represents the three states of the circuit breaker.
type cbState int

const (
	cbClosed   cbState = iota // normal operation
	cbOpen                    // fail-fast, no calls pass through
	cbHalfOpen                // one probe allowed to test recovery
)

func (s cbState) String() string {
	switch s {
	case cbClosed:
		return "closed"
	case cbOpen:
		return "open"
	case cbHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitBreakerConfig holds tuneable parameters for the LLM circuit breaker.
//
// Defaults are sized for long LLM tool chains (up to 5 rounds × ~30 s each):
//   - FailureThreshold is high enough that a single slow-but-valid chain
//     does not trip the breaker.
//   - Interval is wide enough to avoid counting failures from concurrent
//     sessions against each other during normal operation.
//   - OpenTimeout gives the provider enough time to recover between retries.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of consecutive failures that trip the
	// breaker from Closed → Open. Default: 5.
	FailureThreshold int

	// Interval is the rolling time window used to reset the consecutive
	// failure counter when the breaker is Closed. If no failure occurs
	// within this window, the counter resets to zero. Default: 120s.
	//
	// Set wide (≥ max expected single-chain duration) so a slow but
	// successful chain does not leave a stale failure count.
	Interval time.Duration

	// OpenTimeout is how long the breaker stays Open (fail-fast) before
	// transitioning to Half-Open to probe for recovery. Default: 60s.
	OpenTimeout time.Duration

	// MaxHalfOpenRequests is the number of probe calls allowed through
	// while in Half-Open state. Default: 1.
	MaxHalfOpenRequests int
}

// DefaultCircuitBreakerConfig returns conservative defaults suitable for
// a multi-step LLM tool chain where individual turns can take 30+ seconds.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold:    5,
		Interval:            120 * time.Second,
		OpenTimeout:         60 * time.Second,
		MaxHalfOpenRequests: 1,
	}
}

// CircuitBreakerClient wraps any LLMClient with a three-state circuit breaker.
// It implements the LLMClient interface and is a transparent drop-in replacement.
//
// Failure classification: any non-nil error from the inner client's Complete
// call is counted as a provider/transport failure. Validation errors (which
// happen after a successful HTTP call inside callLLM) never reach this layer
// and are therefore not counted.
type CircuitBreakerClient struct {
	inner  LLMClient
	cfg    CircuitBreakerConfig
	logger *slog.Logger

	mu                 sync.Mutex
	state              cbState
	consecutiveFailures int
	halfOpenRequests   int
	lastFailureTime    time.Time
	openedAt           time.Time
}

// NewCircuitBreakerClient wraps inner with a circuit breaker configured by cfg.
func NewCircuitBreakerClient(inner LLMClient, cfg CircuitBreakerConfig, logger *slog.Logger) *CircuitBreakerClient {
	return &CircuitBreakerClient{
		inner:  inner,
		cfg:    cfg,
		logger: logger,
		state:  cbClosed,
	}
}

// Complete executes the LLM call through the circuit breaker.
//
//   - Closed: calls pass through; failures increment the counter.
//   - Open: returns apperrors.ErrLLMCircuitOpen immediately without calling inner.
//   - Half-Open: one probe is allowed; success closes the circuit, failure reopens it.
func (c *CircuitBreakerClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	if err := c.beforeCall(); err != nil {
		return CompletionResponse{}, err
	}

	resp, err := c.inner.Complete(ctx, req)
	c.afterCall(err)
	return resp, err
}

// beforeCall checks whether the call should be allowed through.
// Returns apperrors.ErrLLMCircuitOpen when the breaker is Open.
func (c *CircuitBreakerClient) beforeCall() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.state {
	case cbOpen:
		if time.Since(c.openedAt) >= c.cfg.OpenTimeout {
			c.transitionTo(cbHalfOpen)
			c.halfOpenRequests = 1
			return nil
		}
		return apperrors.ErrLLMCircuitOpen

	case cbHalfOpen:
		if c.halfOpenRequests > 0 {
			c.halfOpenRequests--
			return nil
		}
		// No probe slots left — reject until the current probe resolves.
		return apperrors.ErrLLMCircuitOpen

	default: // cbClosed
		// Reset the failure counter if no failure occurred within the interval.
		if !c.lastFailureTime.IsZero() && time.Since(c.lastFailureTime) > c.cfg.Interval {
			c.consecutiveFailures = 0
		}
		return nil
	}
}

// afterCall records the outcome and drives state transitions.
func (c *CircuitBreakerClient) afterCall(err error) {
	// Ignore circuit-open pseudo-errors (they come from beforeCall, not inner).
	if errors.Is(err, apperrors.ErrLLMCircuitOpen) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		c.recordFailure()
	} else {
		c.recordSuccess()
	}
}

func (c *CircuitBreakerClient) recordFailure() {
	c.lastFailureTime = time.Now()

	switch c.state {
	case cbClosed:
		c.consecutiveFailures++
		if c.consecutiveFailures >= c.cfg.FailureThreshold {
			c.transitionTo(cbOpen)
		}

	case cbHalfOpen:
		// Probe failed — reopen immediately.
		c.transitionTo(cbOpen)
	}
}

func (c *CircuitBreakerClient) recordSuccess() {
	switch c.state {
	case cbClosed:
		c.consecutiveFailures = 0

	case cbHalfOpen:
		// Probe succeeded — close the circuit.
		c.consecutiveFailures = 0
		c.transitionTo(cbClosed)
	}
}

// transitionTo changes state and logs the transition. Must be called with mu held.
func (c *CircuitBreakerClient) transitionTo(next cbState) {
	prev := c.state
	c.state = next
	if next == cbOpen {
		c.openedAt = time.Now()
	}
	c.logger.Warn("llm circuit breaker state change",
		slog.String("from", prev.String()),
		slog.String("to", next.String()),
		slog.Int("consecutive_failures", c.consecutiveFailures),
	)
}
