package matchmaker

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
)

// Evaluator compiles Job requirement expressions and evaluates them against worker advertisements.
type Evaluator struct {
	env   *cel.Env
	mu    sync.RWMutex
	cache map[string]cel.Program
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

	return &Evaluator{
		env:   env,
		cache: make(map[string]cel.Program),
	}, nil
}

// Match evaluates whether a specified job requirement expression is satisfied by the worker ClassAd.
func (e *Evaluator) Match(expression string, adJSON string) (bool, error) {
	e.mu.RLock()
	prg, cached := e.cache[expression]
	e.mu.RUnlock()

	if !cached {
		ast, issues := e.env.Compile(expression)
		if issues != nil && issues.Err() != nil {
			return false, fmt.Errorf("compile error: %w", issues.Err())
		}

		var err error
		prg, err = e.env.Program(ast)
		if err != nil {
			return false, fmt.Errorf("program construction error: %w", err)
		}

		e.mu.Lock()
		e.cache[expression] = prg
		e.mu.Unlock()
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
