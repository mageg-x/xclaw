package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"xclaw/cli/audit"
	"xclaw/cli/db"
	"xclaw/cli/models"
	"xclaw/cli/queue"
)

type Runner func(context.Context, models.CronJob) error

type Scheduler struct {
	store *db.Store
	queue *queue.LaneQueue
	audit *audit.Logger
	run   Runner

	mu          sync.Mutex
	lastTrigger map[string]string
	stopped     chan struct{}
}

func New(store *db.Store, q *queue.LaneQueue, auditLogger *audit.Logger, run Runner) *Scheduler {
	return &Scheduler{
		store:       store,
		queue:       q,
		audit:       auditLogger,
		run:         run,
		lastTrigger: make(map[string]string),
		stopped:     make(chan struct{}),
	}
}

func (s *Scheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		defer ticker.Stop()
		defer close(s.stopped)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.tick(context.Background())
			}
		}
	}()
}

func (s *Scheduler) Stop() {
	<-s.stopped
}

func (s *Scheduler) tick(ctx context.Context) {
	jobs, err := s.store.ListCronJobs(ctx, "", true)
	if err != nil {
		s.audit.Log(ctx, "", "", "cron", "scan_failed", err.Error())
		return
	}

	now := time.Now().UTC()

	for _, job := range jobs {
		nextRun, err := ResolveNextRun(job, now)
		if err != nil {
			s.audit.Log(ctx, job.AgentID, "", "cron", "invalid_schedule", fmt.Sprintf("job=%s err=%v", job.ID, err))
			continue
		}
		if nextRun.IsZero() || now.Before(nextRun) {
			continue
		}

		stamp := nextRun.UTC().Format(time.RFC3339Nano)
		if s.isTriggered(job.ID, stamp) {
			continue
		}
		s.setTriggered(job.ID, stamp)

		nextAfter, stillEnabled, err := ResolveAfterTrigger(job, nextRun, now)
		if err != nil {
			s.audit.Log(ctx, job.AgentID, "", "cron", "resolve_next_failed", fmt.Sprintf("job=%s err=%v", job.ID, err))
		} else {
			_ = s.store.UpdateCronScheduleState(ctx, job.ID, nextAfter, stillEnabled)
			job.NextRunAt = nextAfter
			job.Enabled = stillEnabled
		}

		laneID := s.resolveLane(job)
		local := job
		done := s.queue.Enqueue(context.Background(), laneID, func(taskCtx context.Context) error {
			return s.runJob(taskCtx, local)
		})
		go func() { <-done }()
	}
}

func (s *Scheduler) resolveLane(job models.CronJob) string {
	target := strings.TrimSpace(job.TargetChannel)
	if target != "" && target != "last" {
		return "gateway:" + target
	}
	switch strings.TrimSpace(job.ExecutionMode) {
	case "main":
		return fmt.Sprintf("agent:%s:main", job.AgentID)
	case "custom":
		if strings.TrimSpace(job.SessionID) != "" {
			return "session:" + strings.TrimSpace(job.SessionID)
		}
		return fmt.Sprintf("agent:%s:custom", job.AgentID)
	default:
		return fmt.Sprintf("cron:%s", job.AgentID)
	}
}

func (s *Scheduler) isTriggered(jobID, stamp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTrigger[jobID] == stamp
}

func (s *Scheduler) setTriggered(jobID, stamp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastTrigger[jobID] = stamp
}

func (s *Scheduler) runJob(ctx context.Context, job models.CronJob) error {
	retry := job.RetryLimit
	if retry <= 0 {
		retry = 5
	}
	if retry > 5 {
		retry = 5
	}

	var lastErr error
	backoffSeq := []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second}
	for attempt := 1; attempt <= retry; attempt++ {
		err := s.run(ctx, job)
		if err == nil {
			now := time.Now().UTC()
			_ = s.store.UpdateCronResult(ctx, job.ID, &now, "success", "")
			s.audit.Log(ctx, job.AgentID, "", "cron", "success", fmt.Sprintf("job=%s type=%s mode=%s attempt=%d", job.Name, scheduleType(job.ScheduleType), job.ExecutionMode, attempt))
			return nil
		}

		lastErr = err
		s.audit.Log(ctx, job.AgentID, "", "cron", "retry", fmt.Sprintf("job=%s type=%s mode=%s attempt=%d err=%v", job.Name, scheduleType(job.ScheduleType), job.ExecutionMode, attempt, err))

		if attempt < retry {
			backoff := backoffSeq[attempt-1]
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}

	now := time.Now().UTC()
	errText := ""
	if lastErr != nil {
		errText = lastErr.Error()
	}
	_ = s.store.UpdateCronResult(ctx, job.ID, &now, "failed", errText)
	s.audit.Log(ctx, job.AgentID, "", "cron", "failed", fmt.Sprintf("job=%s type=%s mode=%s err=%s", job.Name, scheduleType(job.ScheduleType), job.ExecutionMode, errText))
	return lastErr
}
