package matchmaker

import (
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
