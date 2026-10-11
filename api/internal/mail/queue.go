package mail

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

var ErrQueueFull = errors.New("mail queue full")

// Queue sends mail asynchronously on a pool of worker goroutines so HTTP handlers never
// block on SMTP. Failed sends are retried with exponential backoff. Close drains the queue.
type Queue struct {
	sender  Sender
	log     *slog.Logger
	jobs    chan Message
	wg      sync.WaitGroup
	retries int
	backoff time.Duration
	closeMu sync.RWMutex
	closed  bool
}

func NewQueue(sender Sender, log *slog.Logger, workers, buffer int) *Queue {
	if workers <= 0 {
		workers = 1
	}
	q := &Queue{sender: sender, log: log, jobs: make(chan Message, buffer), retries: 3, backoff: 2 * time.Second}
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
	return q
}

// Enqueue schedules msg for delivery without blocking.
func (q *Queue) Enqueue(msg Message) error {
	q.closeMu.RLock()
	defer q.closeMu.RUnlock()
	if q.closed {
		return errors.New("mail queue closed")
	}
	select {
	case q.jobs <- msg:
		return nil
	default:
		q.log.Error("mail queue full, dropping message", "subject", msg.Subject)
		return ErrQueueFull
	}
}

func (q *Queue) worker() {
	defer q.wg.Done()
	for msg := range q.jobs {
		q.deliver(msg)
	}
}

func (q *Queue) deliver(msg Message) {
	wait := q.backoff
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := q.sender.Send(ctx, msg)
		cancel()
		if err == nil {
			q.log.Info("email sent", "subject", msg.Subject, "attempt", attempt)
			return
		}
		if attempt > q.retries {
			q.log.Error("email failed", "subject", msg.Subject, "error", err, "attempts", attempt)
			return
		}
		q.log.Warn("email send retry", "subject", msg.Subject, "error", err, "attempt", attempt)
		time.Sleep(wait)
		wait *= 2
	}
}

// Close stops accepting messages and waits (bounded by ctx) for queued mail to be sent.
func (q *Queue) Close(ctx context.Context) error {
	q.closeMu.Lock()
	if !q.closed {
		q.closed = true
		close(q.jobs)
	}
	q.closeMu.Unlock()
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
