package scheduler

import (
	"fmt"
	"strings"
	"time"

	"xclaw/cli/models"
)

func ResolveNextRun(job models.CronJob, now time.Time) (time.Time, error) {
	now = now.UTC()
	if job.NextRunAt != nil && !job.NextRunAt.IsZero() {
		return job.NextRunAt.UTC(), nil
	}

	switch scheduleType(job.ScheduleType) {
	case "at":
		return ParseAtTime(job.Schedule)
	case "every":
		d, err := time.ParseDuration(strings.TrimSpace(job.Schedule))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid duration: %w", err)
		}
		base := job.CreatedAt.UTC()
		if job.LastRunAt != nil && !job.LastRunAt.IsZero() {
			base = job.LastRunAt.UTC()
		}
		next := base.Add(d)
		if next.Before(now) {
			for next.Before(now) {
				next = next.Add(d)
			}
		}
		return next, nil
	default:
		expr, err := parseCron(job.Schedule)
		if err != nil {
			return time.Time{}, err
		}
		currentMinute := now.Truncate(time.Minute)
		if expr.match(currentMinute) {
			return currentMinute, nil
		}
		return expr.Next(now), nil
	}
}

func ResolveAfterTrigger(job models.CronJob, due, now time.Time) (*time.Time, bool, error) {
	switch scheduleType(job.ScheduleType) {
	case "at":
		return nil, false, nil
	case "every":
		d, err := time.ParseDuration(strings.TrimSpace(job.Schedule))
		if err != nil {
			return nil, true, err
		}
		next := due.Add(d)
		for !next.After(now) {
			next = next.Add(d)
		}
		return &next, true, nil
	default:
		expr, err := parseCron(job.Schedule)
		if err != nil {
			return nil, true, err
		}
		next := expr.Next(due)
		if next.IsZero() {
			return nil, true, fmt.Errorf("cannot calculate next run")
		}
		return &next, true, nil
	}
}

func scheduleType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "at":
		return "at"
	case "every":
		return "every"
	case "cron":
		return "cron"
	default:
		return "cron"
	}
}

func ParseAtTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return t.UTC(), nil
		}
		if t, err := time.ParseInLocation(f, raw, time.Local); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid at schedule time: %s", raw)
}
