package agents

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrencyLimiter(t *testing.T) {
	limiter := NewConcurrencyLimiter(2)
	var active int32
	var maxActive int32

	done := make(chan struct{})
	for i := 0; i < 6; i++ {
		go func() {
			limiter.Run(func() {
				cur := atomic.AddInt32(&active, 1)
				for {
					m := atomic.LoadInt32(&maxActive)
					if cur > m {
						if atomic.CompareAndSwapInt32(&maxActive, m, cur) {
							break
						}
					} else {
						break
					}
				}
				time.Sleep(50 * time.Millisecond)
				atomic.AddInt32(&active, -1)
			})
			done <- struct{}{}
		}()
	}

	for i := 0; i < 6; i++ {
		<-done
	}

	if maxActive > 2 {
		t.Fatalf("expected max active <= 2, got %d", maxActive)
	}
}

func TestRunParallel(t *testing.T) {
	ctx := context.Background()
	items := []int{1, 2, 3, 4, 5}

	results, errs := RunParallel(ctx, items, 3, func(c context.Context, item int) (int, error) {
		if item == 3 {
			return 0, errors.New("error on 3")
		}
		return item * 10, nil
	})

	if len(results) != 5 || len(errs) != 5 {
		t.Fatalf("unexpected slice lengths: results=%d, errs=%d", len(results), len(errs))
	}

	if results[0] != 10 || results[1] != 20 || results[3] != 40 || results[4] != 50 {
		t.Fatalf("unexpected results: %+v", results)
	}

	if errs[2] == nil || errs[2].Error() != "error on 3" {
		t.Fatalf("expected error on item 3, got %v", errs[2])
	}
}

func TestRunConcurrentStages(t *testing.T) {
	ctx := context.Background()
	var stage1Done, stage2Done int32

	errs := RunConcurrentStages(ctx,
		func(c context.Context) error {
			time.Sleep(20 * time.Millisecond)
			atomic.StoreInt32(&stage1Done, 1)
			return nil
		},
		func(c context.Context) error {
			time.Sleep(20 * time.Millisecond)
			atomic.StoreInt32(&stage2Done, 1)
			return nil
		},
	)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("stage %d failed: %v", i, err)
		}
	}

	if atomic.LoadInt32(&stage1Done) != 1 || atomic.LoadInt32(&stage2Done) != 1 {
		t.Fatalf("stages did not complete properly")
	}
}
