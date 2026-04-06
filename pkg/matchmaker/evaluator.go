package matchmaker

import (
	"encoding/json"
	"fmt"

	"github.com/google/cel-go/cel"
)

// Evaluator compiles Job requirement expressions and evaluates them against worker advertisements.
type Evaluator struct {
	env *cel.Env
}

// NewEvaluator sets up the Google CEL environment and variables for matchmaking.
func NewEvaluator() (*Evaluator, error) {
	// The "ad" variable represents the worker's attributes
	env, err := cel.NewEnv(
		cel.Variable("ad", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	return &Evaluator{env: env}, nil
}

// Match evaluates whether a specified job requirement expression is satisfied by the worker ClassAd.
func (e *Evaluator) Match(expression string, adJSON string) (bool, error) {
	ast, issues := e.env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("compile error: %w", issues.Err())
	}

	prg, err := e.env.Program(ast)
	if err != nil {
		return false, fmt.Errorf("program construction error: %w", err)
	}

	var adMap map[string]interface{}
	if err := json.Unmarshal([]byte(adJSON), &adMap); err != nil {
		return false, fmt.Errorf("failed to unmarshal class ad: %w", err)
	}

	args := map[string]interface{}{
		"ad": adMap,
	}

	out, _, err := prg.Eval(args)
	if err != nil {
		// Treating evaluation errors (e.g., missing fields) as non-matches to prevent scheduler crash.
		return false, nil
	}

	// Enforce that expressions strictly evaluate to boolean results
	if out.Type() != cel.BoolType {
		return false, fmt.Errorf("expression did not evaluate to a boolean")
	}

	return out.Value().(bool), nil
}
