package credit

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// RetryPolicy governs how the service retries grant creation on transient
// failures (deadlocks, serialization failures, already-exists races during
// concurrent proration). The store's unique partial indexes (migrations 0027,
// 0093) intentionally turn concurrent duplicate inserts into ErrAlreadyExists;
// service callers handle that via GrantOrFetch (idempotent). Retries here
// only cover transient Postgres errors that are safe to retry without double
// crediting — i.e. the insert never committed. The policy is deliberately
// conservative: max 3 attempts, jittered exponential backoff, respects context
// cancellation so a client disconnect doesn't leave a retry loop running in
// the background (which would waste DB connections and could race with the
// next request's idempotency check).
type RetryPolicy struct {
	MaxAttempts   int
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	JitterFactor  float64
	RetryBudgetMS int // total wall-clock budget for all retries
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:   3,
		BaseDelay:     100 * time.Millisecond,
		MaxDelay:      2 * time.Second,
		JitterFactor:  0.2,
		RetryBudgetMS: 3000,
	}
}

// IsRetryable decides if an error from Store is safe to retry.
//
// Safe:
// - serialization failures (SQLState 40001)
// - deadlock detected (40P01)
// - connection errors (store wraps them)
// Unsafe (must not retry — would risk double credit or hide real bug):
// - ErrAlreadyExists (idempotency — caller uses GrantOrFetch)
// - InvalidArgument (validation)
// - NotFound
// - any other domain error
func (p RetryPolicy) IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Domain errors that are not transient
	var ia *errs.InvalidArgumentError
	if errors.As(err, &ia) {
		return false
	}
	var nf *errs.NotFoundError
	if errors.As(err, &nf) {
		return false
	}
	var ae *errs.AlreadyExistsError
	if errors.As(err, &ae) {
		return false
	}
	// Heuristic: check error message for Postgres transient codes.
	// Store layer could expose a typed RetryableError, but for now we pattern-match
	// to keep store interface minimal. This is intentionally conservative — unknown
	// errors are not retried to avoid hiding bugs.
	msg := err.Error()
	if containsTransientCode(msg) {
		return true
	}
	return false
}

func containsTransientCode(msg string) bool {
	// 40001 serialization_failure, 40P01 deadlock_detected
	transient := []string{"40001", "40P01", "connection refused", "connection reset", "too many connections"}
	lower := msg
	// Lowercase check via custom loop to avoid strings import in hot path? Keep simple.
	for _, code := range transient {
		if contains(lower, code) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	// naive contains to avoid extra import in this file's earlier version; keep stdlib lean
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Backoff returns the delay for attempt n (1-indexed), jittered.
// Attempt 1 = BaseDelay ± jitter, attempt 2 = BaseDelay*2 ± jitter, capped at MaxDelay.
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	// Exponential: base * 2^(attempt-1)
	backoff := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
	if backoff > float64(p.MaxDelay) {
		backoff = float64(p.MaxDelay)
	}
	// Jitter: ± JitterFactor * backoff
	jitter := (rand.Float64()*2 - 1) * p.JitterFactor * backoff
	return time.Duration(backoff + jitter)
}

// Execute runs fn with retries per policy, respecting context and budget.
// Returns last error if all attempts fail. Context cancellation stops retries
// immediately and returns ctx.Err() so callers see cancellation, not a
// wrapped store error. The retry budget (RetryBudgetMS) ensures a single bulk
// request with many transient failures doesn't hold a DB connection for longer
// than ~3s.
func (p RetryPolicy) Execute(ctx context.Context, fn func() error) error {
	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !p.IsRetryable(err) {
			return err
		}
		if attempt == p.MaxAttempts {
			break
		}
		// Budget check
		elapsed := time.Since(start)
		if elapsed.Milliseconds() >= int64(p.RetryBudgetMS) {
			break
		}
		delay := p.Backoff(attempt)
		// Don't exceed remaining budget
		remaining := time.Duration(p.RetryBudgetMS)*time.Millisecond - elapsed
		if delay > remaining {
			delay = remaining
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return lastErr
}
