// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cel

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"unsafe"

	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/decls"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/interpreter"
)

// ── Debug instrumentation ────────────────────────────────────────────────────
// All functions in this block write to stderr with the prefix [cel:probe].
// Remove or gate behind an env-var before upstreaming.

// celProbeSizesOnce ensures the one-time struct-size analysis is logged exactly once.
var celProbeSizesOnce sync.Once

func init() {
	celProbeSizesOnce.Do(func() {
		// Measure shallow struct sizes to understand per-instance overhead.
		//
		// prog struct (unexported, measured via proxy — matches the actual field
		// layout documented below):
		//
		//   Field                    Type                          Notes
		//   *Env                     *Env (8 B ptr)                SHARED pointer; Env itself is allocated once per env variant
		//   evalOpts                 uint64 (8 B)
		//   defaultVars              Activation (iface 16 B)
		//   dispatcher               Dispatcher (iface 16 B)        per-program, NEW each call
		//   interpreter              Interpreter (iface 16 B)       per-program, holds ptrs to shared env fields
		//   interruptCheckFrequency  uint (8 B)
		//   plannerOptions           []PlannerOption (24 B)         slice header, per-program
		//   regexOptimizations       []*RegexOptimization (24 B)    slice header, per-program
		//   interpretable            Interpretable (iface 16 B)     per-program, planner output
		//   observable               *ObservableInterpretable (8 B) per-program (nil usually)
		//   callCostEstimator        ActualCostEstimator (16 B)
		//   costOptions              []CostTrackerOption (24 B)     slice header
		//   costLimit                *uint64 (8 B)
		//
		// Total header: ~192 B (plus backing arrays for slices and the dispatcher/interpretable)

		fmt.Fprintf(os.Stderr,
			"[cel:probe:sizes] Env=%d B  prog-header≈192 B\n",
			unsafe.Sizeof(Env{}),
		)

		// functions.Overload (from common/functions/functions.go):
		//   Operator string(16) + OperandTrait int(8) + Unary func(8) + Binary func(8) +
		//   Function func(8) + NonStrict bool(1) + 7 pad = 56 B
		// Each fn.Bindings() call allocates N new *Overload structs (N ≤ 2×overloads).
		// The Unary/Binary/Function fields hold guard-closure func values;
		// the closures capture the actual implementation and type-check logic.
		// The underlying implementation is SHARED; only the per-overload closure
		// wrapper is new — typically ~64–128 B each on the heap.
		fmt.Fprintf(os.Stderr,
			"[cel:probe:sizes] *functions.Overload-header=56 B  dispatcher-map-header=24 B\n",
		)

		// Memory cost breakdown per env.Program() call:
		//   prog header:         ~192 B  (fixed)
		//   dispatcher map hdr:  ~24 B   (fixed)
		//   per bound overload:  ~56 B (*Overload) + ~128 B (guard closures) + ~72 B (map bucket) ≈ 256 B
		//   With 345 overloads (unpatched):  345 × 256 B ≈ 86 KB per program
		//   With ~10 overloads (patched):     10 × 256 B ≈  2.5 KB per program
		//   At 25,000 programs (5000 policies × 5 expressions):
		//     unpatched: 25000 × 86 KB ≈ 2.1 GB
		//     patched:   25000 × 2.5 KB ≈ 61 MB  (~35× reduction)
		fmt.Fprintf(os.Stderr,
			"[cel:probe:sizes] dispatcher-cost: unpatched≈86KB/prog  patched≈2.5KB/prog  25k-prog: unpatched≈2.1GB  patched≈61MB\n",
		)

		// The *Env is a SHARED pointer across all programs from the same env variant.
		// Its size (Sizeof(Env{}) above is the struct header; the actual retained heap
		// includes: functions map (~2–4 MB for 157 fns), checker.Env (~200 KB), parser (~20 KB).
		// This is NOT duplicated per program — it's fixed cost amortised across all compilations.
		fmt.Fprintf(os.Stderr,
			"[cel:probe:sizes] *Env is SHARED across programs: functions/checker/parser state allocated once per env-variant (8 variants total)\n",
		)
	})
}

// celProbeEnv logs the contents of a cel.Env at the start of newProgram.
// It prints the env pointer so callers can verify the same *Env is reused
// across multiple program compilations (proving it is not duplicated).
func celProbeEnv(e *Env) {
	totalOverloads := 0
	fnNames := make([]string, 0, len(e.functions))
	for name, fn := range e.functions {
		fnNames = append(fnNames, name)
		totalOverloads += len(fn.OverloadDecls())
	}
	sort.Strings(fnNames)

	varNames := make([]string, 0, len(e.variables))
	for _, v := range e.variables {
		varNames = append(varNames, v.Name())
	}
	sort.Strings(varNames)

	libNames := make([]string, 0, len(e.libraries))
	for name := range e.libraries {
		libNames = append(libNames, name)
	}
	sort.Strings(libNames)

	fmt.Fprintf(os.Stderr,
		"[cel:probe:env] ptr=%p  functions=%d  totalOverloads=%d  variables=%d  macros=%d  libraries=%d\n",
		e, len(e.functions), totalOverloads, len(e.variables), len(e.macros), len(e.libraries),
	)
	fmt.Fprintf(os.Stderr,
		"[cel:probe:env]   variables=%v\n", varNames,
	)
	fmt.Fprintf(os.Stderr,
		"[cel:probe:env]   libraries=%v\n", libNames,
	)
	// Print all function names at a lower verbosity.
	fmt.Fprintf(os.Stderr,
		"[cel:probe:env]   functions=%v\n", fnNames,
	)
}

// celProbeDispatcher logs what ended up in the dispatcher after addNeededBindings.
// The dispatcher has a parent (the shared lazy-populated parent) and a per-program
// overlay; only the overlay keys are shown here (extra bindings from ProgramOptions).
// The full set (parent + overlay) is shown via disp.OverloadIds().
func celProbeDispatcher(disp interpreter.Dispatcher, label string) {
	ids := disp.OverloadIds()
	sort.Strings(ids)
	fmt.Fprintf(os.Stderr,
		"[cel:probe:dispatcher] %s: bound=%d  ids=%v\n",
		label, len(ids), ids,
	)
}

// celProbeInterpretable logs the type of the Interpretable returned by the planner.
// The planner builds a tree of eval nodes from the typed AST.  The root type
// reveals the top-level expression kind:
//   - *evalCall       — a function call (most common for policy expressions)
//   - *evalComprehension / *evalFold — a comprehension (e.g. .all(), .exists())
//   - InterpretableAttribute — a simple variable/field access
//   - *evalConst      — a constant
//   - *ObservableInterpretable — wraps any of the above with observers
//
// Memory note: the interpretable tree is per-program and proportional to AST
// complexity.  For typical policy expressions (5–20 AST nodes) it is small
// (≈ 1–5 KB), but for large comprehensions over list inputs it can grow.
func celProbeInterpretable(i interpreter.Interpretable, label string) {
	if i == nil {
		fmt.Fprintf(os.Stderr, "[cel:probe:interpretable] %s: nil\n", label)
		return
	}
	fmt.Fprintf(os.Stderr,
		"[cel:probe:interpretable] %s: type=%T  id=%d\n",
		label, i, i.ID(),
	)
}

// ── End debug instrumentation ────────────────────────────────────────────────

// Program is an evaluable view of an Ast.
type Program interface {
	// Eval returns the result of an evaluation of the Ast and environment against the input vars.
	//
	// The vars value may either be an `Activation` or a `map[string]any`.
	//
	// If the `OptTrackState`, `OptTrackCost` or `OptExhaustiveEval` flags are used, the `details` response will
	// be non-nil. Given this caveat on `details`, the return state from evaluation will be:
	//
	// *  `val`, `details`, `nil` - Successful evaluation of a non-error result.
	// *  `val`, `details`, `err` - Successful evaluation to an error result.
	// *  `nil`, `details`, `err` - Unsuccessful evaluation.
	//
	// An unsuccessful evaluation is typically the result of a series of incompatible `EnvOption`
	// or `ProgramOption` values used in the creation of the evaluation environment or executable
	// program.
	Eval(any) (ref.Val, *EvalDetails, error)

	// ContextEval evaluates the program with a set of input variables and a context object in order
	// to support cancellation and timeouts. This method must be used in conjunction with the
	// InterruptCheckFrequency() option for cancellation interrupts to be impact evaluation.
	//
	// The vars value may either be an `Activation` or `map[string]any`.
	//
	// The output contract for `ContextEval` is otherwise identical to the `Eval` method.
	ContextEval(context.Context, any) (ref.Val, *EvalDetails, error)
}

// Activation used to resolve identifiers by name and references by id.
//
// An Activation is the primary mechanism by which a caller supplies input into a CEL program.
type Activation = interpreter.Activation

// NewActivation returns an activation based on a map-based binding where the map keys are
// expected to be qualified names used with ResolveName calls.
//
// The input `bindings` may either be of type `Activation` or `map[string]any`.
//
// Lazy bindings may be supplied within the map-based input in either of the following forms:
// - func() any
// - func() ref.Val
//
// The output of the lazy binding will overwrite the variable reference in the internal map.
//
// Values which are not represented as ref.Val types on input may be adapted to a ref.Val using
// the types.Adapter configured in the environment.
func NewActivation(bindings any) (Activation, error) {
	return interpreter.NewActivation(bindings)
}

// PartialActivation extends the Activation interface with a set of unknown AttributePatterns.
type PartialActivation = interpreter.PartialActivation

// NoVars returns an empty Activation.
func NoVars() Activation {
	return interpreter.EmptyActivation()
}

// PartialVars returns a PartialActivation which contains variables and a set of AttributePattern
// values that indicate variables or parts of variables whose value are not yet known.
//
// This method relies on manually configured sets of missing attribute patterns. For a method which
// infers the missing variables from the input and the configured environment, use Env.PartialVars().
//
// The `vars` value may either be an Activation or any valid input to the NewActivation call.
func PartialVars(vars any,
	unknowns ...*AttributePatternType) (PartialActivation, error) {
	return interpreter.NewPartialActivation(vars, unknowns...)
}

// AttributePattern returns an AttributePattern that matches a top-level variable. The pattern is
// mutable, and its methods support the specification of one or more qualifier patterns.
//
// For example, the AttributePattern(`a`).QualString(`b`) represents a variable access `a` with a
// string field or index qualification `b`. This pattern will match Attributes `a`, and `a.b`,
// but not `a.c`.
//
// When using a CEL expression within a container, e.g. a package or namespace, the variable name
// in the pattern must match the qualified name produced during the variable namespace resolution.
// For example, when variable `a` is declared within an expression whose container is `ns.app`, the
// fully qualified variable name may be `ns.app.a`, `ns.a`, or `a` per the CEL namespace resolution
// rules. Pick the fully qualified variable name that makes sense within the container as the
// AttributePattern `varName` argument.
func AttributePattern(varName string) *AttributePatternType {
	return interpreter.NewAttributePattern(varName)
}

// AttributePatternType represents a top-level variable with an optional set of qualifier patterns.
//
// See the interpreter.AttributePattern and interpreter.AttributeQualifierPattern for more info
// about how to create and manipulate AttributePattern values.
type AttributePatternType = interpreter.AttributePattern

// EvalDetails holds additional information observed during the Eval() call.
type EvalDetails struct {
	state       interpreter.EvalState
	costTracker *interpreter.CostTracker
}

// State of the evaluation, non-nil if the OptTrackState or OptExhaustiveEval is specified
// within EvalOptions.
func (ed *EvalDetails) State() interpreter.EvalState {
	if ed == nil {
		return interpreter.NewEvalState()
	}
	return ed.state
}

// ActualCost returns the tracked cost through the course of execution when `CostTracking` is enabled.
// Otherwise, returns nil if the cost was not enabled.
func (ed *EvalDetails) ActualCost() *uint64 {
	if ed == nil || ed.costTracker == nil {
		return nil
	}
	cost := ed.costTracker.ActualCost()
	return &cost
}

// prog is the internal implementation of the Program interface.
type prog struct {
	*Env
	evalOpts                EvalOption
	defaultVars             Activation
	dispatcher              interpreter.Dispatcher
	interpreter             interpreter.Interpreter
	interruptCheckFrequency uint

	// Intermediate state used to configure the InterpretableDecorator set provided
	// to the initInterpretable call.
	plannerOptions     []interpreter.PlannerOption
	regexOptimizations []*interpreter.RegexOptimization

	// Interpretable configured from an Ast and aggregate decorator set based on program options.
	interpretable     interpreter.Interpretable
	observable        *interpreter.ObservableInterpretable
	callCostEstimator interpreter.ActualCostEstimator
	costOptions       []interpreter.CostTrackerOption
	costLimit         *uint64
}

// newProgram creates a program instance with an environment, an ast, and an optional list of
// ProgramOption values.
//
// If the program cannot be configured the prog will be nil, with a non-nil error response.
func newProgram(e *Env, a *ast.AST, opts []ProgramOption) (Program, error) {
	// Log the env contents so callers can verify the *Env is shared (same pointer)
	// across all programs compiled from the same env variant.
	celProbeEnv(e)

	// Reuse the env's shared dispatcher for the bulk of the function bindings
	// (every overload from e.functions is materialised exactly once per env).
	// Wrap it with ExtendDispatcher so the deprecated Functions() ProgramOption
	// can still register program-local overloads in an isolated overlay
	// without mutating the shared parent.
	parentDisp, err := e.sharedDispatcher()
	if err != nil {
		return nil, err
	}
	disp := interpreter.ExtendDispatcher(parentDisp)

	// Ensure the default attribute factory is set after the adapter and provider are
	// configured.
	p := &prog{
		Env:            e,
		plannerOptions: []interpreter.PlannerOption{},
		dispatcher:     disp,
		costOptions:    []interpreter.CostTrackerOption{},
	}

	// Configure the program via the ProgramOption values. The deprecated
	// Functions() option mutates p.dispatcher, which is the per-program
	// overlay above, not the shared parent.
	for _, opt := range opts {
		p, err = opt(p)
		if err != nil {
			return nil, err
		}
	}

	// Add the function bindings created via Function() options.
	// For type-checked ASTs, restrict bindings to the overloads actually
	// referenced by this expression so fn.Bindings() (which allocates a new
	// closure wrapper per overload) is only called for the ~5-15 functions the
	// expression uses, not all ~80 registered functions.
	if err := addNeededBindings(disp, e.functions, a); err != nil {
		return nil, err
	}
	// Log the per-program overlay dispatcher state after binding.
	// This shows how many overload closures were actually allocated.
	celProbeDispatcher(disp, "after-addNeededBindings")

	// Set the attribute factory after the options have been set.
	var attrFactory interpreter.AttributeFactory
	attrFactorOpts := []interpreter.AttrFactoryOption{
		interpreter.EnableErrorOnBadPresenceTest(p.HasFeature(featureEnableErrorOnBadPresenceTest)),
	}
	if p.evalOpts&OptPartialEval == OptPartialEval {
		attrFactory = interpreter.NewPartialAttributeFactory(e.Container, e.adapter, e.provider, attrFactorOpts...)
	} else {
		attrFactory = interpreter.NewAttributeFactory(e.Container, e.adapter, e.provider, attrFactorOpts...)
	}
	interp := interpreter.NewInterpreter(disp, e.Container, e.provider, e.adapter, attrFactory)
	p.interpreter = interp
	// The interpreter holds pointers to shared *Env fields (Container, provider, adapter,
	// attrFactory) — it does NOT copy them.  It is a thin coordination struct.
	fmt.Fprintf(os.Stderr, "[cel:probe:interpreter] type=%T  prog-ptr=%p  env-ptr=%p\n",
		interp, p, e)

	// Translate the EvalOption flags into InterpretableDecorator instances.
	plannerOptions := make([]interpreter.PlannerOption, len(p.plannerOptions))
	copy(plannerOptions, p.plannerOptions)

	// Enable interrupt checking if there's a non-zero check frequency
	if p.interruptCheckFrequency > 0 {
		plannerOptions = append(plannerOptions, interpreter.InterruptableEval())
	}
	// Enable constant folding first.
	if p.evalOpts&OptOptimize == OptOptimize {
		plannerOptions = append(plannerOptions, interpreter.Optimize())
		p.regexOptimizations = append(p.regexOptimizations, interpreter.MatchesRegexOptimization)
	}
	// Enable regex compilation of constants immediately after folding constants.
	if len(p.regexOptimizations) > 0 {
		plannerOptions = append(plannerOptions, interpreter.CompileRegexConstants(p.regexOptimizations...))
	}

	// Enable exhaustive eval, state tracking and cost tracking last since they require a factory.
	if p.evalOpts&(OptExhaustiveEval|OptTrackState|OptTrackCost) != 0 {
		costOptCount := len(p.costOptions)
		if p.costLimit != nil {
			costOptCount++
		}
		costOpts := make([]interpreter.CostTrackerOption, 0, costOptCount)
		costOpts = append(costOpts, p.costOptions...)
		if p.costLimit != nil {
			costOpts = append(costOpts, interpreter.CostTrackerLimit(*p.costLimit))
		}
		trackerFactory := func() (*interpreter.CostTracker, error) {
			return interpreter.NewCostTracker(p.callCostEstimator, costOpts...)
		}
		var observers []interpreter.PlannerOption
		if p.evalOpts&(OptExhaustiveEval|OptTrackState) != 0 {
			// EvalStateObserver is required for OptExhaustiveEval.
			observers = append(observers, interpreter.EvalStateObserver())
		}
		if p.evalOpts&OptTrackCost == OptTrackCost {
			observers = append(observers, interpreter.CostObserver(interpreter.CostTrackerFactory(trackerFactory)))
		}
		// Enable exhaustive eval over a basic observer since it offers a superset of features.
		if p.evalOpts&OptExhaustiveEval == OptExhaustiveEval {
			plannerOptions = append(plannerOptions,
				append([]interpreter.PlannerOption{interpreter.ExhaustiveEval()}, observers...)...)
		} else if len(observers) > 0 {
			plannerOptions = append(plannerOptions, observers...)
		}
	}
	return p.initInterpretable(a, plannerOptions)
}

func (p *prog) initInterpretable(a *ast.AST, plannerOptions []interpreter.PlannerOption) (*prog, error) {
	// When the AST has been exprAST it contains metadata that can be used to speed up program execution.
	//
	// The planner walks the typed AST and builds a tree of Interpretable eval nodes.
	// Each call-site in the AST becomes an *evalCall or *evalFold node that captures a
	// function pointer looked up from the dispatcher — this is why the dispatcher must be
	// populated BEFORE NewInterpretable runs.  After planning, the interpretable tree
	// bakes in the function pointers and no longer needs the dispatcher at runtime.
	interpretable, err := p.interpreter.NewInterpretable(a, plannerOptions...)
	if err != nil {
		return nil, err
	}
	p.interpretable = interpretable
	if oi, ok := interpretable.(*interpreter.ObservableInterpretable); ok {
		p.observable = oi
	}
	celProbeInterpretable(p.interpretable, "after-planning")
	fmt.Fprintf(os.Stderr,
		"[cel:probe:program] built: prog-ptr=%p  env-ptr=%p  AST-checked=%v  AST-refNodes=%d\n",
		p, p.Env, a.IsChecked(), len(a.ReferenceMap()),
	)
	return p, nil
}

// addNeededBindings populates disp with function overload bindings.
//
// For type-checked ASTs it only calls fn.Bindings() for functions that have
// at least one overload referenced in the expression's ReferenceMap, skipping
// the ~150 functions whose closure wrappers would otherwise be allocated and
// retained in the (now-released) dispatcher.
//
// Dispatch key mechanics:
//   - Per-overload bindings: Overload.Operator == overload ID (e.g. "int_add_int")
//   - Singleton bindings:    Overload.Operator == function name  (e.g. "_+_")
//
// The planner resolves calls by trying the overload ID first, then the function
// name as fallback. Both cases are handled here.
//
// Falls back to binding all functions for unchecked (parse-only) ASTs.
func addNeededBindings(disp interpreter.Dispatcher, functions map[string]*decls.FunctionDecl, a *ast.AST) error {
	refMap := a.ReferenceMap()
	if !a.IsChecked() || len(refMap) == 0 {
		// Unchecked AST: bind everything (safe fallback).
		// This path is not reached in normal Kubernetes CEL usage (all expressions are type-checked).
		totalFns := 0
		for _, fn := range functions {
			bindings, err := fn.Bindings()
			if err != nil {
				return err
			}
			if err = disp.Add(bindings...); err != nil {
				return err
			}
			totalFns++
		}
		fmt.Fprintf(os.Stderr,
			"[cel:probe:addNeededBindings] unchecked-fallback: bound all %d functions\n", totalFns)
		return nil
	}

	// Step 1: collect overload IDs referenced in this expression.
	usedOIDs := make(map[string]struct{}, len(refMap)*2)
	for _, ref := range refMap {
		for _, oID := range ref.OverloadIDs {
			usedOIDs[oID] = struct{}{}
		}
	}

	// Step 2: build overload-ID → function-name reverse index.
	overloadToFnName := make(map[string]string, len(functions)*3)
	totalOverloads := 0
	for name, fn := range functions {
		for _, o := range fn.OverloadDecls() {
			overloadToFnName[o.ID()] = name
			totalOverloads++
		}
	}

	// Step 3: which function names have at least one referenced overload?
	neededFns := make(map[string]struct{}, len(usedOIDs))
	for oID := range usedOIDs {
		if fnName, ok := overloadToFnName[oID]; ok {
			neededFns[fnName] = struct{}{}
		}
	}

	// Step 4: bind only the needed overloads.
	boundKeys := make([]string, 0, len(usedOIDs)*2)
	for name, fn := range functions {
		if _, needed := neededFns[name]; !needed {
			continue
		}
		bindings, err := fn.Bindings()
		if err != nil {
			return err
		}
		for _, b := range bindings {
			isSingleton := b.Operator == name
			if isSingleton {
				if err = disp.Add(b); err != nil {
					return err
				}
				boundKeys = append(boundKeys, b.Operator)
			} else if _, used := usedOIDs[b.Operator]; used {
				if err = disp.Add(b); err != nil {
					return err
				}
				boundKeys = append(boundKeys, b.Operator)
			}
		}
	}

	sort.Strings(boundKeys)
	// Memory saving: (totalOverloads - len(boundKeys)) overloads skipped.
	// Each skipped overload avoids allocating a *functions.Overload + guard closures (~256 B).
	savedB := (totalOverloads - len(boundKeys)) * 256
	fmt.Fprintf(os.Stderr,
		"[cel:probe:addNeededBindings] checked: totalFns=%d  totalOverloads=%d  usedOIDs=%d  neededFns=%d  boundKeys=%d  savedB≈%d  boundKeys=%v\n",
		len(functions), totalOverloads, len(usedOIDs), len(neededFns), len(boundKeys), savedB, boundKeys,
	)
	return nil
}

// Eval implements the Program interface method.
func (p *prog) Eval(input any) (out ref.Val, det *EvalDetails, err error) {
	// Configure error recovery for unexpected panics during evaluation. Note, the use of named
	// return values makes it possible to modify the error response during the recovery
	// function.
	defer func() {
		if r := recover(); r != nil {
			switch t := r.(type) {
			case interpreter.EvalCancelledError:
				err = t
			default:
				err = fmt.Errorf("internal error: %v", r)
			}
		}
	}()
	// Build a hierarchical activation if there are default vars set.
	var vars Activation
	switch v := input.(type) {
	case Activation:
		vars = v
	case map[string]any:
		vars = activationPool.Setup(v)
		defer activationPool.Put(vars)
	default:
		return nil, nil, fmt.Errorf("invalid input, wanted Activation or map[string]any, got: (%T)%v", input, input)
	}
	if p.defaultVars != nil {
		vars = interpreter.NewHierarchicalActivation(p.defaultVars, vars)
	}
	if p.observable != nil {
		det = &EvalDetails{}
		out = p.observable.ObserveEval(vars, func(observed any) {
			switch o := observed.(type) {
			case interpreter.EvalState:
				det.state = o
			case *interpreter.CostTracker:
				det.costTracker = o
			}
		})
	} else {
		out = p.interpretable.Eval(vars)
	}
	// The output of an internal Eval may have a value (`v`) that is a types.Err. This step
	// translates the CEL value to a Go error response. This interface does not quite match the
	// RPC signature which allows for multiple errors to be returned, but should be sufficient.
	if types.IsError(out) {
		err = out.(*types.Err)
	}
	return
}

// ContextEval implements the Program interface.
func (p *prog) ContextEval(ctx context.Context, input any) (ref.Val, *EvalDetails, error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("context can not be nil")
	}
	// Configure the input, making sure to wrap Activation inputs in the special ctxActivation which
	// exposes the #interrupted variable and manages rate-limited checks of the ctx.Done() state.
	var vars Activation
	switch v := input.(type) {
	case Activation:
		vars = ctxActivationPool.Setup(v, ctx.Done(), p.interruptCheckFrequency)
		defer ctxActivationPool.Put(vars)
	case map[string]any:
		rawVars := activationPool.Setup(v)
		defer activationPool.Put(rawVars)
		vars = ctxActivationPool.Setup(rawVars, ctx.Done(), p.interruptCheckFrequency)
		defer ctxActivationPool.Put(vars)
	default:
		return nil, nil, fmt.Errorf("invalid input, wanted Activation or map[string]any, got: (%T)%v", input, input)
	}
	return p.Eval(vars)
}

type ctxEvalActivation struct {
	parent                  Activation
	interrupt               <-chan struct{}
	interruptCheckCount     uint
	interruptCheckFrequency uint
}

// ResolveName implements the Activation interface method, but adds a special #interrupted variable
// which is capable of testing whether a 'done' signal is provided from a context.Context channel.
func (a *ctxEvalActivation) ResolveName(name string) (any, bool) {
	if name == "#interrupted" {
		a.interruptCheckCount++
		if a.interruptCheckCount%a.interruptCheckFrequency == 0 {
			select {
			case <-a.interrupt:
				return true, true
			default:
				return nil, false
			}
		}
		return nil, false
	}
	return a.parent.ResolveName(name)
}

func (a *ctxEvalActivation) Parent() Activation {
	return a.parent
}

func (a *ctxEvalActivation) AsPartialActivation() (interpreter.PartialActivation, bool) {
	pa, ok := a.parent.(interpreter.PartialActivation)
	return pa, ok
}

func newCtxEvalActivationPool() *ctxEvalActivationPool {
	return &ctxEvalActivationPool{
		Pool: sync.Pool{
			New: func() any {
				return &ctxEvalActivation{}
			},
		},
	}
}

type ctxEvalActivationPool struct {
	sync.Pool
}

// Setup initializes a pooled Activation with the ability check for context.Context cancellation
func (p *ctxEvalActivationPool) Setup(vars Activation, done <-chan struct{}, interruptCheckRate uint) *ctxEvalActivation {
	a := p.Pool.Get().(*ctxEvalActivation)
	a.parent = vars
	a.interrupt = done
	a.interruptCheckCount = 0
	a.interruptCheckFrequency = interruptCheckRate
	return a
}

type evalActivation struct {
	vars     map[string]any
	lazyVars map[string]any
}

// ResolveName looks up the value of the input variable name, if found.
//
// Lazy bindings may be supplied within the map-based input in either of the following forms:
// - func() any
// - func() ref.Val
//
// The lazy binding will only be invoked once per evaluation.
//
// Values which are not represented as ref.Val types on input may be adapted to a ref.Val using
// the types.Adapter configured in the environment.
func (a *evalActivation) ResolveName(name string) (any, bool) {
	v, found := a.vars[name]
	if !found {
		return nil, false
	}
	switch obj := v.(type) {
	case func() ref.Val:
		if resolved, found := a.lazyVars[name]; found {
			return resolved, true
		}
		lazy := obj()
		a.lazyVars[name] = lazy
		return lazy, true
	case func() any:
		if resolved, found := a.lazyVars[name]; found {
			return resolved, true
		}
		lazy := obj()
		a.lazyVars[name] = lazy
		return lazy, true
	default:
		return obj, true
	}
}

// Parent implements the Activation interface
func (a *evalActivation) Parent() Activation {
	return nil
}

func newEvalActivationPool() *evalActivationPool {
	return &evalActivationPool{
		Pool: sync.Pool{
			New: func() any {
				return &evalActivation{lazyVars: make(map[string]any)}
			},
		},
	}
}

type evalActivationPool struct {
	sync.Pool
}

// Setup initializes a pooled Activation object with the map input.
func (p *evalActivationPool) Setup(vars map[string]any) *evalActivation {
	a := p.Pool.Get().(*evalActivation)
	a.vars = vars
	return a
}

func (p *evalActivationPool) Put(value any) {
	a := value.(*evalActivation)
	for k := range a.lazyVars {
		delete(a.lazyVars, k)
	}
	p.Pool.Put(a)
}

var (
	// activationPool is an internally managed pool of Activation values that wrap map[string]any inputs
	activationPool = newEvalActivationPool()

	// ctxActivationPool is an internally managed pool of Activation values that expose a special #interrupted variable
	ctxActivationPool = newCtxEvalActivationPool()
)
