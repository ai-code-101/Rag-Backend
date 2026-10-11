// Package ratelimit implements fixed-window rate limiting with in-memory and Redis
// backends. Use Redis when running more than one API instance so limits are global.
package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Rule is "Limit events per Window".
type Rule struct {
	Limit  int
	Window time.Duration
}

type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

type Limiter interface {
	Allow(ctx context.Context, key string, rule Rule) (Result, error)
}

// Memory is a process-local limiter. Expired windows are swept periodically.
type Memory struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	count int
	reset time.Time
}

func NewMemory() *Memory {
	m := &Memory{buckets: map[string]*bucket{}, now: time.Now}
	go m.sweep()
	return m
}

func (m *Memory) Allow(_ context.Context, key string, rule Rule) (Result, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buckets[key]
	if !ok || !now.Before(b.reset) {
		b = &bucket{reset: now.Add(rule.Window)}
		m.buckets[key] = b
	}
	if b.count >= rule.Limit {
		return Result{Allowed: false, RetryAfter: b.reset.Sub(now)}, nil
	}
	b.count++
	return Result{Allowed: true, Remaining: rule.Limit - b.count}, nil
}

func (m *Memory) sweep() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		now := m.now()
		m.mu.Lock()
		for k, b := range m.buckets {
			if !now.Before(b.reset) {
				delete(m.buckets, k)
			}
		}
		m.mu.Unlock()
	}
}

// Redis is a shared limiter using INCR + PEXPIRE in a single round trip.
type Redis struct {
	client *redis.Client
	prefix string
}

func NewRedis(url string) (*Redis, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	return &Redis{client: redis.NewClient(opt), prefix: "rl:"}, nil
}

var incrScript = redis.NewScript(`
local c = redis.call('INCR', KEYS[1])
if c == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return {c, redis.call('PTTL', KEYS[1])}`)

func (r *Redis) Allow(ctx context.Context, key string, rule Rule) (Result, error) {
	res, err := incrScript.Run(ctx, r.client, []string{r.prefix + key}, rule.Window.Milliseconds()).Int64Slice()
	if err != nil {
		return Result{Allowed: true}, err // fail open on Redis outage; the error is logged by the caller
	}
	count, ttl := int(res[0]), time.Duration(res[1])*time.Millisecond
	if count > rule.Limit {
		return Result{Allowed: false, RetryAfter: ttl}, nil
	}
	return Result{Allowed: true, Remaining: rule.Limit - count}, nil
}

func (r *Redis) Close() error { return r.client.Close() }
