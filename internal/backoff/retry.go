package backoff

import "time"

// Delay returns how long to wait before retry number `attempt`.
// Doubles each time, up to maxDelay.
func Delay(attempt int, base, maxDelay time.Duration) time.Duration {
	d := base
	for i := 0; i < attempt; i++ {
		d = d * 2
	}
	if d > maxDelay {
		return maxDelay
	}
	return d
}

// ShouldRetry reports whether another attempt is allowed.
func ShouldRetry(attempt, maxAttempts int) bool {
	return attempt <= maxAttempts
}
