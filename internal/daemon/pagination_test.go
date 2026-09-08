package daemon

import (
	"testing"
	"time"

	"github.com/deziss/tasch/pkg/scheduler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func jobsForPaging(n int) []*scheduler.Job {
	jobs := make([]*scheduler.Job, n)
	base := time.Now()
	for i := range jobs {
		jobs[i] = &scheduler.Job{ID: string(rune('a' + i%26)), SubmitTime: base.Add(time.Duration(i) * time.Second)}
	}
	return jobs
}

// TestPaginateWalksEveryJobExactlyOnce is the property that matters: following the tokens must
// visit every job, with none skipped and none repeated.
func TestPaginateWalksEveryJobExactlyOnce(t *testing.T) {
	jobs := jobsForPaging(250)

	seen := 0
	token := ""
	pages := 0
	for {
		page, next, err := paginate(jobs, token, 40)
		if err != nil {
			t.Fatalf("paginate: %v", err)
		}
		seen += len(page)
		pages++
		if next == "" {
			break
		}
		token = next
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
	}

	if seen != len(jobs) {
		t.Errorf("walked %d jobs, want %d", seen, len(jobs))
	}
	if pages != 7 { // 250 / 40 = 6.25, so 7 pages
		t.Errorf("took %d pages, want 7", pages)
	}
}

func TestPaginateAppliesDefaultAndMaximum(t *testing.T) {
	jobs := jobsForPaging(maxPageSize + 500)

	page, _, err := paginate(jobs, "", 0)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if len(page) != defaultPageSize {
		t.Errorf("default page = %d jobs, want %d", len(page), defaultPageSize)
	}

	// A caller asking for more than the maximum is capped rather than served an unbounded
	// response, which is the failure this pagination exists to prevent.
	page, _, err = paginate(jobs, "", maxPageSize*10)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if len(page) != maxPageSize {
		t.Errorf("oversized request returned %d jobs, want the %d cap", len(page), maxPageSize)
	}
}

func TestPaginateLastPageHasNoToken(t *testing.T) {
	jobs := jobsForPaging(10)

	page, next, err := paginate(jobs, "", 10)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if len(page) != 10 {
		t.Errorf("page = %d jobs, want 10", len(page))
	}
	if next != "" {
		t.Errorf("next token = %q, want empty on an exactly-full final page", next)
	}
}

func TestPaginateEmptyAndOutOfRange(t *testing.T) {
	page, next, err := paginate(nil, "", 10)
	if err != nil || len(page) != 0 || next != "" {
		t.Errorf("empty input gave (%d jobs, %q, %v)", len(page), next, err)
	}

	jobs := jobsForPaging(5)
	page, next, err = paginate(jobs, "999", 10)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if len(page) != 0 || next != "" {
		t.Errorf("past-the-end token gave (%d jobs, %q)", len(page), next)
	}
}

func TestPaginateRejectsBadToken(t *testing.T) {
	jobs := jobsForPaging(5)
	for _, token := range []string{"abc", "-1", "1.5"} {
		if _, _, err := paginate(jobs, token, 10); err == nil {
			t.Errorf("paginate accepted an invalid token %q", token)
		} else if status.Code(err) != codes.InvalidArgument {
			t.Errorf("token %q: code = %s, want InvalidArgument", token, status.Code(err))
		}
	}
}
