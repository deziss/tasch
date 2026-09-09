package daemon

import (
	"strings"
	"testing"
)

func TestParseArraySpec(t *testing.T) {
	tests := []struct {
		spec        string
		wantIndices []int
		wantLimit   int
	}{
		{spec: "5", wantIndices: []int{5}},
		{spec: "1-4", wantIndices: []int{1, 2, 3, 4}},
		{spec: "0-8:4", wantIndices: []int{0, 4, 8}},
		{spec: "3,1,2", wantIndices: []int{1, 2, 3}},
		{spec: "1-3,7", wantIndices: []int{1, 2, 3, 7}},
		// Overlapping components are deduplicated rather than producing two tasks with the
		// same index, which would run the same work twice.
		{spec: "1-3,2-4", wantIndices: []int{1, 2, 3, 4}},
		{spec: "1-100%5", wantIndices: nil, wantLimit: 5},
		{spec: " 1 - 3 ", wantIndices: []int{1, 2, 3}},
	}

	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := parseArraySpec(tc.spec)
			if err != nil {
				t.Fatalf("parseArraySpec(%q) = %v", tc.spec, err)
			}
			if got.MaxConcurrent != tc.wantLimit {
				t.Fatalf("MaxConcurrent = %d, want %d", got.MaxConcurrent, tc.wantLimit)
			}
			if tc.wantIndices == nil {
				return
			}
			if len(got.Indices) != len(tc.wantIndices) {
				t.Fatalf("indices = %v, want %v", got.Indices, tc.wantIndices)
			}
			for i := range tc.wantIndices {
				if got.Indices[i] != tc.wantIndices[i] {
					t.Fatalf("indices = %v, want %v", got.Indices, tc.wantIndices)
				}
			}
		})
	}
}

func TestParseArraySpecRejects(t *testing.T) {
	tests := []struct {
		spec    string
		wantErr string
	}{
		{spec: "", wantErr: "is empty"},
		{spec: "abc", wantErr: "is not a number"},
		{spec: "-1", wantErr: "is not a number"},
		{spec: "9-1", wantErr: "ends before it starts"},
		{spec: "1-3:0", wantErr: "step must be at least 1"},
		{spec: "1-3%0", wantErr: "limit must be at least 1"},
		{spec: "1-3%x", wantErr: "is not a number"},
		// A typo in the upper bound must be refused, not turned into ten million jobs.
		{spec: "1-10000000", wantErr: "more than"},
	}

	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			_, err := parseArraySpec(tc.spec)
			if err == nil {
				t.Fatalf("parseArraySpec(%q) = nil, want an error containing %q", tc.spec, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("parseArraySpec(%q) = %q, want it to contain %q", tc.spec, err, tc.wantErr)
			}
		})
	}
}
