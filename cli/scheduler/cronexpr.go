package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type cronExpr struct {
	minute []int
	hour   []int
	dom    []int
	month  []int
	dow    []int
}

func parseCron(expression string) (cronExpr, error) {
	parts := strings.Fields(strings.TrimSpace(expression))
	if len(parts) == 6 {
		// 兼容秒级 6 字段表达式，当前调度器按分钟精度运行，忽略秒字段。
		parts = parts[1:]
	}
	if len(parts) != 5 {
		return cronExpr{}, fmt.Errorf("invalid cron expression: expected 5 or 6 fields")
	}

	minute, err := parseField(parts[0], 0, 59)
	if err != nil {
		return cronExpr{}, fmt.Errorf("minute: %w", err)
	}
	hour, err := parseField(parts[1], 0, 23)
	if err != nil {
		return cronExpr{}, fmt.Errorf("hour: %w", err)
	}
	dom, err := parseField(parts[2], 1, 31)
	if err != nil {
		return cronExpr{}, fmt.Errorf("dom: %w", err)
	}
	month, err := parseField(parts[3], 1, 12)
	if err != nil {
		return cronExpr{}, fmt.Errorf("month: %w", err)
	}
	dow, err := parseField(parts[4], 0, 6)
	if err != nil {
		return cronExpr{}, fmt.Errorf("dow: %w", err)
	}

	return cronExpr{minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

func (c cronExpr) match(t time.Time) bool {
	return contains(c.minute, t.Minute()) &&
		contains(c.hour, t.Hour()) &&
		contains(c.dom, t.Day()) &&
		contains(c.month, int(t.Month())) &&
		contains(c.dow, int(t.Weekday()))
}

func (c cronExpr) Next(from time.Time) time.Time {
	t := from.UTC().Truncate(time.Minute).Add(time.Minute)
	deadline := t.AddDate(5, 0, 0)
	for !t.After(deadline) {
		if c.match(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

func parseField(field string, min, max int) ([]int, error) {
	if field == "*" {
		out := make([]int, 0, max-min+1)
		for i := min; i <= max; i++ {
			out = append(out, i)
		}
		return out, nil
	}

	values := make(map[int]struct{})
	segments := strings.Split(field, ",")
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}

		if strings.HasPrefix(seg, "*/") {
			step, err := strconv.Atoi(strings.TrimPrefix(seg, "*/"))
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("invalid step %s", seg)
			}
			for i := min; i <= max; i += step {
				values[i] = struct{}{}
			}
			continue
		}

		if strings.Contains(seg, "/") {
			baseStep := strings.SplitN(seg, "/", 2)
			if len(baseStep) != 2 {
				return nil, fmt.Errorf("invalid step segment %s", seg)
			}
			step, err := strconv.Atoi(strings.TrimSpace(baseStep[1]))
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("invalid step %s", seg)
			}
			base := strings.TrimSpace(baseStep[0])
			if base == "*" {
				for i := min; i <= max; i += step {
					values[i] = struct{}{}
				}
				continue
			}
			if strings.Contains(base, "-") {
				bound := strings.SplitN(base, "-", 2)
				if len(bound) != 2 {
					return nil, fmt.Errorf("invalid range step %s", seg)
				}
				start, err := strconv.Atoi(strings.TrimSpace(bound[0]))
				if err != nil {
					return nil, fmt.Errorf("invalid range start %s", seg)
				}
				end, err := strconv.Atoi(strings.TrimSpace(bound[1]))
				if err != nil {
					return nil, fmt.Errorf("invalid range end %s", seg)
				}
				if start < min || end > max || start > end {
					return nil, fmt.Errorf("range out of bounds %s", seg)
				}
				for i := start; i <= end; i += step {
					values[i] = struct{}{}
				}
				continue
			}
			return nil, fmt.Errorf("invalid step base %s", seg)
		}

		if strings.Contains(seg, "-") {
			bound := strings.SplitN(seg, "-", 2)
			if len(bound) != 2 {
				return nil, fmt.Errorf("invalid range %s", seg)
			}
			start, err := strconv.Atoi(bound[0])
			if err != nil {
				return nil, fmt.Errorf("invalid range start %s", seg)
			}
			end, err := strconv.Atoi(bound[1])
			if err != nil {
				return nil, fmt.Errorf("invalid range end %s", seg)
			}
			if start < min || end > max || start > end {
				return nil, fmt.Errorf("range out of bounds %s", seg)
			}
			for i := start; i <= end; i++ {
				values[i] = struct{}{}
			}
			continue
		}

		v, err := strconv.Atoi(seg)
		if err != nil {
			return nil, fmt.Errorf("invalid value %s", seg)
		}
		if v < min || v > max {
			return nil, fmt.Errorf("value out of bounds %d", v)
		}
		values[v] = struct{}{}
	}

	if len(values) == 0 {
		return nil, fmt.Errorf("empty field")
	}

	out := make([]int, 0, len(values))
	for i := min; i <= max; i++ {
		if _, ok := values[i]; ok {
			out = append(out, i)
		}
	}
	return out, nil
}

func contains(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
