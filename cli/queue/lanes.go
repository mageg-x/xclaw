package queue

import (
	"context"
	"fmt"
	"sync"
)

type Task func(context.Context) error

type LaneQueue struct {
	globalTokens chan struct{}

	mu     sync.Mutex
	lanes  map[string]chan taskEnvelope
	closed bool
}

type taskEnvelope struct {
	ctx  context.Context
	task Task
	done chan error
}

func NewLaneQueue(globalConcurrency int) *LaneQueue {
	if globalConcurrency <= 0 {
		globalConcurrency = 16
	}
	return &LaneQueue{
		globalTokens: make(chan struct{}, globalConcurrency),
		lanes:        make(map[string]chan taskEnvelope),
	}
}

func (q *LaneQueue) Enqueue(ctx context.Context, laneID string, task Task) <-chan error {
	done := make(chan error, 1)

	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		done <- fmt.Errorf("lane queue closed")
		return done
	}

	lane, ok := q.lanes[laneID]
	if !ok {
		lane = make(chan taskEnvelope, 128)
		q.lanes[laneID] = lane
		go q.runLane(laneID, lane)
	}
	q.mu.Unlock()

	env := taskEnvelope{ctx: ctx, task: task, done: done}
	select {
	case lane <- env:
	case <-ctx.Done():
		done <- ctx.Err()
	}
	return done
}

func (q *LaneQueue) runLane(laneID string, lane chan taskEnvelope) {
	for env := range lane {
		select {
		case q.globalTokens <- struct{}{}:
		case <-env.ctx.Done():
			env.done <- env.ctx.Err()
			continue
		}

		err := env.task(env.ctx)
		<-q.globalTokens
		env.done <- err
		close(env.done)
	}

	q.mu.Lock()
	delete(q.lanes, laneID)
	q.mu.Unlock()
}

func (q *LaneQueue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	for _, lane := range q.lanes {
		close(lane)
	}
	q.mu.Unlock()
}
