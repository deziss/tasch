package matchmaker

import (
	"fmt"
	"testing"
)

func TestEvaluatorMatch(t *testing.T) {
	eval, err := NewEvaluator()
	if err != nil {
		t.Fatalf("Failed to create evaluator: %v", err)
	}

	adJSON := `{
		"os": "linux",
		"architecture": "amd64",
		"cpu_cores": 12,
		"available_mem_mb": 4096,
		"host_type": "vm_or_baremetal"
	}`

	tests := []struct {
		name       string
		expression string
		adJSON     string
		wantMatch  bool
		wantErr    bool
	}{
		{
			name:       "Basic memory requirement",
			expression: "ad.available_mem_mb >= 2048",
			adJSON:     adJSON,
			wantMatch:  true,
			wantErr:    false,
		},
		{
			name:       "Memory lacking",
			expression: "ad.available_mem_mb >= 8192",
			adJSON:     adJSON,
			wantMatch:  false,
			wantErr:    false,
		},
		{
			name:       "Complex requirement",
			expression: "ad.os == 'linux' && ad.cpu_cores >= 8 && ad.host_type == 'vm_or_baremetal'",
			adJSON:     adJSON,
			wantMatch:  true,
			wantErr:    false,
		},
		{
			name:       "Missing field handling",
			expression: "ad.gpu_cores > 0", // field doesn't exist; eval returns error -> Match returns false, nil
			adJSON:     adJSON,
			wantMatch:  false,
			wantErr:    false,
		},
		{
			name:       "Compilation error",
			expression: "return ad", // Invalid syntax
			adJSON:     adJSON,
			wantMatch:  false,
			wantErr:    true, // Must throw syntactical compile error
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eval.Match(tc.expression, tc.adJSON)
			if (err != nil) != tc.wantErr {
				t.Errorf("Match() err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.wantMatch {
				t.Errorf("Match() got = %v, wantMatch %v", got, tc.wantMatch)
			}
		})
	}
}

// TestValidateRejectsBadRequirements confirms a malformed or non-boolean requirement is caught
// at submit time. Compile errors used to surface only inside the dispatch loop, which discarded
// them, so such a job sat QUEUED forever with no diagnostic anywhere.
func TestValidateRejectsBadRequirements(t *testing.T) {
	e, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}

	cases := []struct {
		name       string
		expression string
		wantErr    bool
	}{
		{"valid", `ad.gpu_count >= 2`, false},
		{"valid compound", `ad.gpu_count >= 1 && ad.os == "linux"`, false},
		{"literal true", `true`, false},
		{"empty", ``, true},
		{"syntax error", `ad.gpu_count >=`, true},
		{"unknown variable", `node.gpu_count >= 1`, true},
		{"not a boolean", `ad.gpu_count`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := e.Validate(tc.expression)
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want an error", tc.expression)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tc.expression, err)
			}
		})
	}
}

// TestCacheIsBounded confirms the compiled-program cache cannot grow without limit from a
// stream of distinct requirement strings.
func TestCacheIsBounded(t *testing.T) {
	e, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	ad := `{"gpu_count": 4}`

	for i := 0; i < maxCachedPrograms+50; i++ {
		expr := fmt.Sprintf("ad.gpu_count >= %d", i)
		if _, err := e.Match(expr, ad); err != nil {
			t.Fatalf("Match(%q): %v", expr, err)
		}
	}

	e.mu.RLock()
	size := len(e.cache)
	e.mu.RUnlock()
	if size > maxCachedPrograms {
		t.Errorf("cache holds %d programs, over the %d bound", size, maxCachedPrograms)
	}
}
