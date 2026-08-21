package backoff

import "time"

// Delay returns how long to wait before retry number `attempt`.
// Doubles each time, up to maxDelay. Clamps inside the loop so a large
// attempt cannot overflow time.Duration and wrap negative.
func Delay(attempt int, base, maxDelay time.Duration) time.Duration {
	if attempt < 0 {
		return base
	}
	d := base
	for i := 0; i < attempt; i++ {
		d = d * 2
		if d > maxDelay || d < base {
			return maxDelay
		}
	}
	if d > maxDelay {
		return maxDelay
	}
	return d
}

// ShouldRetry reports whether another attempt is allowed.
// attempt is 0-indexed, so maxAttempts=3 permits attempts 0, 1 and 2.
func ShouldRetry(attempt, maxAttempts int) bool {
	return attempt < maxAttempts
}
