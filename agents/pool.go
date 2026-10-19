package agents

import (
	"context"
	"sync"
)

// ConcurrencyLimiter limits concurrent operations (e.g. LLM calls or API requests).
type ConcurrencyLimiter struct {
	sem chan struct{}
}

// NewConcurrencyLimiter creates a limiter with capacity maxConcurrent.
func NewConcurrencyLimiter(maxConcurrent int) *ConcurrencyLimiter {
	if maxConcurrent <= 0 {
		maxConcurrent = 3 // default safe concurrency limit
	}
	return &ConcurrencyLimiter{
		sem: make(chan struct{}, maxConcurrent),
	}
}

// Acquire reserves one concurrency slot.
func (l *ConcurrencyLimiter) Acquire() {
	l.sem <- struct{}{}
}

// Release frees one concurrency slot.
func (l *ConcurrencyLimiter) Release() {
	<-l.sem
}

// Run executes fn inside the concurrency limiter.
func (l *ConcurrencyLimiter) Run(fn func()) {
	l.Acquire()
	defer l.Release()
	fn()
}

// RunParallel runs worker on each item in items concurrently, bounded by maxConcurrent.
func RunParallel[T any, R any](
	ctx context.Context,
	items []T,
	maxConcurrent int,
	worker func(ctx context.Context, item T) (R, error),
) ([]R, []error) {
	if len(items) == 0 {
		return nil, nil
	}

	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}

	limiter := NewConcurrencyLimiter(maxConcurrent)
	results := make([]R, len(items))
	errs := make([]error, len(items))

	var wg sync.WaitGroup
	for i := range items {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			limiter.Acquire()
			defer limiter.Release()

			select {
			case <-ctx.Done():
				errs[idx] = ctx.Err()
				return
			default:
			}

			res, err := worker(ctx, items[idx])
			results[idx] = res
			errs[idx] = err
		}(i)
	}

	wg.Wait()
	return results, errs
}

// RunConcurrentStages executes multiple independent functions concurrently and waits for completion.
func RunConcurrentStages(ctx context.Context, stages ...func(ctx context.Context) error) []error {
	if len(stages) == 0 {
		return nil
	}

	errs := make([]error, len(stages))
	var wg sync.WaitGroup

	for i, stage := range stages {
		wg.Add(1)
		go func(idx int, fn func(context.Context) error) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				errs[idx] = ctx.Err()
				return
			default:
			}
			errs[idx] = fn(ctx)
		}(i, stage)
	}

	wg.Wait()
	return errs
}
