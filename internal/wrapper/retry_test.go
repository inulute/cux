package wrapper

import (
	"testing"
	"time"
)

func TestFibonacciDelay(t *testing.T) {
	want := []time.Duration{
		10 * time.Second, // attempt 0
		10 * time.Second,
		20 * time.Second,
		30 * time.Second,
		50 * time.Second,
		80 * time.Second,
		2 * time.Minute, // capped from here on
		2 * time.Minute,
	}
	for n, exp := range want {
		if got := fibonacciDelay(n); got != exp {
			t.Errorf("fibonacciDelay(%d) = %v, want %v", n, got, exp)
		}
	}
	// Large n must stay capped and not overflow.
	if got := fibonacciDelay(1000); got != 2*time.Minute {
		t.Errorf("fibonacciDelay(1000) = %v, want %v", got, 2*time.Minute)
	}
}
