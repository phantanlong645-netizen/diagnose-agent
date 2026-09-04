package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestMapReducerCancelsRemainingMapsAfterFirstError(t *testing.T) {
	firstErr := errors.New("first map error")
	var started atomic.Int32
	var exited atomic.Int32
	var reduceCalled atomic.Bool

	result, err := (&MapReducer{
		MaxShardChars: 1,
		MaxParallel:   3,
		MapFn: func(ctx context.Context, shard string, index int) (string, error) {
			started.Add(1)
			defer exited.Add(1)
			if index == 0 {
				return "", firstErr
			}
			<-ctx.Done()
			return "", ctx.Err()
		},
		ReduceFn: func(context.Context, []string) (string, error) {
			reduceCalled.Store(true)
			return "reduced", nil
		},
	}).Run(context.Background(), "a\n\nb\n\nc")

	if !errors.Is(err, firstErr) {
		t.Fatalf("expected first map error, got %v", err)
	}
	if result != "" {
		t.Fatalf("expected empty result, got %q", result)
	}
	if started.Load() != 3 || exited.Load() != 3 {
		t.Fatalf("expected all three workers to start and exit, started=%d exited=%d", started.Load(), exited.Load())
	}
	if reduceCalled.Load() {
		t.Fatal("reduce must not run after a map error")
	}
}
