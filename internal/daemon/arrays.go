package daemon

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Array job specifications.
//
// An array is one submission that expands into many jobs differing only by an index. The point
// is not to save typing — a shell loop does that — it is that the scheduler ends up holding the
// whole set at once, so it can throttle how many run concurrently, report them as a unit, and
// keep them together in the queue instead of interleaving a thousand independent submissions
// with everyone else's work.

// maxArrayTasks bounds a single expansion. A typo like "1-10000000" would otherwise try to
// build ten million jobs before the queue-size check ever ran.
const maxArrayTasks = 100000

// arraySpec is a parsed array specification.
type arraySpec struct {
	// Indices are the task indices, sorted and deduplicated.
	Indices []int
	// MaxConcurrent caps how many tasks of the array may run at once. 0 means no cap.
	MaxConcurrent int
}

// parseArraySpec reads Slurm-style array syntax: "1-100", "1-100:2" for a step, "1,4,7" for an
// explicit list, any combination of those separated by commas, and an optional "%N" suffix
// capping concurrency.
func parseArraySpec(spec string) (*arraySpec, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("array specification is empty")
	}

	out := &arraySpec{}
	if body, limit, found := strings.Cut(spec, "%"); found {
		n, err := strconv.Atoi(strings.TrimSpace(limit))
		if err != nil {
			return nil, fmt.Errorf("array concurrency limit %q is not a number", limit)
		}
		if n < 1 {
			return nil, fmt.Errorf("array concurrency limit must be at least 1 (got %d)", n)
		}
		out.MaxConcurrent = n
		spec = body
	}

	seen := make(map[int]bool)
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		indices, err := parseArrayRange(part)
		if err != nil {
			return nil, err
		}
		for _, i := range indices {
			if seen[i] {
				continue
			}
			seen[i] = true
			out.Indices = append(out.Indices, i)
			if len(out.Indices) > maxArrayTasks {
				return nil, fmt.Errorf("array would expand to more than %d tasks", maxArrayTasks)
			}
		}
	}

	if len(out.Indices) == 0 {
		return nil, fmt.Errorf("array specification %q selects no tasks", spec)
	}
	sort.Ints(out.Indices)
	return out, nil
}

// parseArrayRange expands one comma-free component: "7", "1-9", or "1-9:2".
func parseArrayRange(part string) ([]int, error) {
	step := 1
	if body, stepStr, found := strings.Cut(part, ":"); found {
		n, err := strconv.Atoi(strings.TrimSpace(stepStr))
		if err != nil {
			return nil, fmt.Errorf("array step %q is not a number", stepStr)
		}
		if n < 1 {
			return nil, fmt.Errorf("array step must be at least 1 (got %d)", n)
		}
		step = n
		part = body
	}

	lo, hi, isRange := strings.Cut(part, "-")
	start, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return nil, fmt.Errorf("array index %q is not a number", lo)
	}
	if start < 0 {
		return nil, fmt.Errorf("array indices cannot be negative (got %d)", start)
	}
	if !isRange {
		return []int{start}, nil
	}

	end, err := strconv.Atoi(strings.TrimSpace(hi))
	if err != nil {
		return nil, fmt.Errorf("array index %q is not a number", hi)
	}
	if end < start {
		return nil, fmt.Errorf("array range %q ends before it starts", part)
	}
	if (end-start)/step+1 > maxArrayTasks {
		return nil, fmt.Errorf("array range %q would expand to more than %d tasks", part, maxArrayTasks)
	}

	var out []int
	for i := start; i <= end; i += step {
		out = append(out, i)
	}
	return out, nil
}
