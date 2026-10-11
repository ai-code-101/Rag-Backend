package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestMemoryFixedWindow(t *testing.T) {
	m := &Memory{buckets: map[string]*bucket{}}
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	rule := Rule{Limit: 3, Window: time.Minute}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if r, _ := m.Allow(ctx, "k", rule); !r.Allowed {
			t.Fatalf("request %d denied", i)
		}
	}
	r, _ := m.Allow(ctx, "k", rule)
	if r.Allowed || r.RetryAfter != time.Minute {
		t.Fatalf("4th request should be denied with retry-after 1m: %+v", r)
	}
	if r, _ := m.Allow(ctx, "other", rule); !r.Allowed {
		t.Fatal("keys must be independent")
	}
	now = now.Add(time.Minute)
	if r, _ := m.Allow(ctx, "k", rule); !r.Allowed {
		t.Fatal("window should reset")
	}
}
