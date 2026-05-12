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
	"github.com/google/cel-go/common/functions"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/interpreter"
)

// ── Debug probe instrumentation ──────────────────────────────────────────────
// All functions below write to stderr with the prefix [cel:probe].
// Remove or gate behind an environment variable before upstreaming.

// celProbeSizesOnce ensures the startup struct-size analysis fires exactly once.
var celProbeSizesOnce sync.Once

func init() {
	celProbeSizesOnce.Do(func() {
		// ── Shallow struct sizes ─────────────────────────────────────────────
		//
		// prog struct (field-by-field layout):
		//   *Env                     8 B ptr  ← SHARED pointer; Env itself built once per variant
		//   evalOpts      uint64     8 B
		//   defaultVars   Activation 16 B (interface)
		//   dispatcher    Dispatcher 16 B (interface)       ← per-program, NEW each call
		//   interpreter   Interpreter 16 B (interface)      ← per-program, ptrs to shared env fields
		//   interruptCheckFrequency uint 8 B
		//   plannerOptions []PlannerOption  24 B slice hdr  ← per-program
		//   regexOptimizations []*Regex     24 B slice hdr  ← per-program
		//   interpretable  Interpretable   16 B (interface) ← per-program, planner output tree
		//   observable     *Observable      8 B ptr
		//   callCostEstimator               16 B (interface)
		//   costOptions    []CostOpt        24 B slice hdr
		//   costLimit      *uint64           8 B ptr
		//   Total header ≈ 192 B (+ backing arrays for slices, map, interpretable tree)
		fmt.Fprintf(os.Stderr,
			"[cel:probe:sizes] Env-struct=%d B  prog-header≈192 B\n",
			unsafe.Sizeof(Env{}),
		)

		// functions.Overload (common/functions/functions.go):
		//   Operator string(16) + OperandTrait int(8) + Unary func(8)
		//   + Binary func(8) + Function func(8) + NonStrict bool(1) + 7 pad = 56 B struct
		//   Guard closures allocated by fn.Bindings() add ~200 B on the heap per overload.
		//   Total per bound overload ≈ 256 B.
		fmt.Fprintf(os.Stderr,
			"[cel:probe:sizes] functions.Overload struct=56 B  total-with-closures≈256 B  dispatcher-map-hdr=24 B\n",
		)

		// Scaling summary:
		//   Without addNeededBindings: 345 overloads × 256 B ≈ 88 KB per program
		//   With    addNeededBindings:  ~10 overloads × 256 B ≈  2.6 KB per program
		//   At 25,000 programs (5000 policies × 5 expressions each):
		//     unpatched: 25000 × 88 KB ≈ 2.1 GB  dispatcher overhead alone
		//     patched:   25000 × 2.6 KB ≈ 63 MB  (~33× reduction)
		fmt.Fprintf(os.Stderr,
			"[cel:probe:sizes] dispatcher scaling: unpatched≈88KB/prog(2.1GB@25k)  patched≈2.6KB/prog(63MB@25k)\n",
		)

		// The *Env embed is a SHARED pointer — functions/checker/parser/adapter/provider
		// are allocated once per env variant and NOT duplicated per program.
		fmt.Fprintf(os.Stderr,
			"[cel:probe:sizes] *Env is SHARED: ~2–4 MB functions + ~200KB checker + ~30KB parser built once per variant (8 variants)\n",
		)
	})
}

// celProbeEnv logs the *Env contents at the start of newProgram so callers can verify
// the same pointer is reused across all programs from the same env variant.
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
	fmt.Fprintf(os.Stderr, "[cel:probe:env]   variables=%v\n", varNames)
	fmt.Fprintf(os.Stderr, "[cel:probe:env]   libraries=%v\n", libNames)
	fmt.Fprintf(os.Stderr, "[cel:probe:env]   functions=%v\n", fnNames)
}

// celProbeDispatcher logs the overload IDs bound into the per-program dispatcher
// overlay after addNeededBindings runs.  Compare bound count vs totalOverloads from
// celProbeEnv to see the savings.
func celProbeDispatcher(disp interpreter.Dispatcher, label string) {
	ids := disp.OverloadIds()
	sort.Strings(ids)
	fmt.Fprintf(os.Stderr,
		"[cel:probe:dispatcher] %s: bound=%d  ids=%v\n", label, len(ids), ids,
	)
}

// celProbeInterpretable logs the concrete type of the Interpretable tree root.
//
// The planner builds a tree of eval nodes from the typed AST; the root type
// reveals the top-level expression kind:
//   - *evalCall                 — function/operator call (most common in policy expressions)
//   - *evalComprehension / *evalFold — comprehension (.all(), .exists(), etc.)
//   - InterpretableAttribute    — simple variable/field access
//   - *evalConst                — constant literal
//   - *ObservableInterpretable  — wraps any of the above when observers are registered
//
// The interpretable tree is per-program, proportional to AST complexity.
// For typical policy expressions (5–20 AST nodes) it is small (~1–5 KB);
// for large comprehensions over list inputs it grows proportionally.
func celProbeInterpretable(i interpreter.Interpretable, label string) {
	if i == nil {
		fmt.Fprintf(os.Stderr, "[cel:probe:interpretable] %s: nil\n", label)
		return
	}
	fmt.Fprintf(os.Stderr,
		"[cel:probe:interpretable] %s: type=%T  id=%d\n", label, i, i.ID(),
	)
}

// ── End debug probe instrumentation ──────────────────────────────────────────

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
	// Log the env pointer so callers can verify the same *Env is reused across all
	// programs compiled from the same env variant — proving it is not duplicated.
	celProbeEnv(e)

	// Build the per-program dispatcher.
	disp := interpreter.NewDispatcher()

	// Ensure the default attribute factory is set after the adapter and provider are
	// configured.
	p := &prog{
		Env:            e,
		plannerOptions: []interpreter.PlannerOption{},
		dispatcher:     disp,
		costOptions:    []interpreter.CostTrackerOption{},
	}

	// Configure the program via the ProgramOption values.
	var err error
	for _, opt := range opts {
		p, err = opt(p)
		if err != nil {
			return nil, err
		}
	}

	// Compute the BEFORE stats: total declared overloads across all registered
	// functions.  This is what the old code would have bound unconditionally.
	// We count from OverloadDecls() (no allocation) rather than calling Bindings().
	totalEnvOverloads := 0
	for _, fn := range e.functions {
		totalEnvOverloads += len(fn.OverloadDecls())
	}
	// fn.Bindings() adds ~2% extra entries for single-overload functions
	// (overload-ID entry + function-name fallback entry).  Measured: 345 → 352.
	wouldHaveBound := totalEnvOverloads * 1021 / 1000

	// addNeededBindings filters fn.Bindings() calls to only the functions that
	// have at least one overload referenced in this expression's AST ReferenceMap.
	// For unchecked ASTs it falls back to binding everything (safe).
	if err := addNeededBindings(disp, e.functions, a); err != nil {
		return nil, err
	}
	actuallyBound := len(disp.OverloadIds())
	savedB := (wouldHaveBound - actuallyBound) * 320

	fmt.Fprintf(os.Stderr,
		"[cel:probe:bindings] addNeededBindings: wouldHaveBound=%d  actuallyBound=%d  savedB≈%d  savedPct=%d%%\n",
		wouldHaveBound, actuallyBound, savedB,
		(wouldHaveBound-actuallyBound)*100/max(wouldHaveBound, 1),
	)
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
	// The interpreter is a thin struct that holds POINTERS to shared *Env fields
	// (Container, provider, adapter, attrFactory).  It does NOT copy those fields.
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
	// The planner walks the typed AST and builds a tree of Interpretable eval nodes.
	// Each call-site in the AST becomes an *evalCall or *evalFold node that captures a
	// function pointer looked up from the dispatcher — the dispatcher must therefore be
	// fully populated BEFORE NewInterpretable runs.  After planning, the interpretable
	// tree bakes in the function closures directly; the dispatcher is not needed at eval time.
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

// addNeededBindings populates disp with the minimum set of *functions.Overload
// entries needed to evaluate this expression, by filtering fn.Bindings() calls
// to only functions that the type-checker resolved for the expression.
//
// ── Background: the unoptimised flow ────────────────────────────────────────
//   The original code calls fn.Bindings() for EVERY registered function, then
//   adds ALL returned *functions.Overload entries to the dispatcher map.
//   Cost: 157 fns → 352 dispatcher entries, each holding guard closures.
//   Size: ~320 B/entry × 352 = ~112 KB allocated per program.
//   A typical policy expression needs only 5–15 of those entries.
//
// ── Data structures involved ─────────────────────────────────────────────────
//
//   ast.ReferenceMap  map[int64]*ast.ReferenceInfo
//     Each call-site in the typed AST maps to a ReferenceInfo whose
//     OverloadIDs field lists the overload IDs the type-checker resolved.
//     This is the ground truth for what the expression needs at runtime.
//
//   decls.FunctionDecl  (e.functions map[string]*FunctionDecl)
//     Registered in the cel.Env.  Has Name() and OverloadDecls() (no alloc).
//     fn.Bindings() allocates and returns []*functions.Overload.
//
//   functions.Overload  (result of fn.Bindings())
//     The RUNTIME representation of one dispatched implementation.
//     Fields:
//       Operator     string           ← the dispatcher map key
//       Unary        UnaryOp          ← set if 1-arg guarded closure, else nil
//       Binary       BinaryOp         ← set if 2-arg guarded closure, else nil
//       Function     FunctionOp       ← set if variadic / multi-dispatch closure
//       OperandTrait int              ← trait check on first arg (0 = any type)
//       NonStrict    bool             ← called even when args are error/unknown
//
//   interpreter.Dispatcher  (dispatcher.go)
//     A simple map[string]*functions.Overload.
//     Add(b) does: d.overloads[b.Operator] = b  (panics if duplicate key)
//     FindOverload(key) looks up d.overloads[key].
//     The planner calls FindOverload(overloadID) at plan time; the resulting
//     function pointer is baked into the interpretable tree.  The dispatcher
//     is NOT used at eval time — only at plan time.
//
// ── What fn.Bindings() produces ─────────────────────────────────────────────
//   Case A: singleton function (f.singleton != nil)
//     → 1 entry keyed by function name.
//   Case B: single-overload function where overloadID == functionName
//     → 1 entry keyed by overloadID (same as function name).
//   Case C: single-overload function where overloadID != functionName
//     → 2 entries: one keyed by overloadID + one keyed by functionName (fallback).
//   Case D: multi-overload function (N overloads)
//     → N per-overload entries (each keyed by overloadID) +
//       1 top-level funcDispatch entry keyed by functionName.
//   Case E: no binding (declaration-only, no implementation)
//     → 0 entries.
//
// ── Dispatch safety ──────────────────────────────────────────────────────────
//   The planner first tries FindOverload(overloadID); if that misses it tries
//   FindOverload(functionName).  We always register both so both paths work.
//
// ── Safety fallback ──────────────────────────────────────────────────────────
//   For unchecked ASTs (no ReferenceMap) we fall back to binding everything.
//   This path is not exercised in normal Kubernetes CEL usage.
func addNeededBindings(disp interpreter.Dispatcher, fns map[string]*decls.FunctionDecl, a *ast.AST) error {
	refMap := a.ReferenceMap()
	if !a.IsChecked() || len(refMap) == 0 {
		total := 0
		for _, fn := range fns {
			bindings, err := fn.Bindings()
			if err != nil {
				return err
			}
			if err = disp.Add(bindings...); err != nil {
				return err
			}
			total++
		}
		fmt.Fprintf(os.Stderr,
			"[cel:probe:addNeededBindings] UNCHECKED-FALLBACK: bound all %d functions (no AST type info)\n", total)
		return nil
	}

	// ── Step 1: collect used OIDs from the type-checker reference map ────────
	// ast.ReferenceMap maps each call-site node ID → ReferenceInfo.
	// ReferenceInfo.OverloadIDs = the overload IDs the type-checker resolved.
	// These are the ONLY keys the planner will request from the dispatcher.
	usedOIDs := make(map[string]struct{}, len(refMap)*2)
	for _, ref := range refMap {
		for _, oID := range ref.OverloadIDs {
			usedOIDs[oID] = struct{}{}
		}
	}
	sortedUsedOIDs := sortedStringSet(usedOIDs)
	fmt.Fprintf(os.Stderr,
		"[cel:probe:ast:oids] type-checker resolved %d unique overload IDs for this expression: %v\n",
		len(usedOIDs), sortedUsedOIDs)

	// ── Step 2: build reverse index overload-ID → function-name ─────────────
	// OverloadDecls() returns the declared overloads without allocating Overload
	// closures — this is safe to call for all 157 functions.
	overloadToFnName := make(map[string]string, len(fns)*3)
	totalDeclaredOverloads := 0
	for name, fn := range fns {
		for _, o := range fn.OverloadDecls() {
			overloadToFnName[o.ID()] = name
			totalDeclaredOverloads++
		}
	}

	// Log each used OID with the function it maps to (or UNKNOWN if not found).
	// This confirms the type-checker's resolution is consistent with what's registered.
	for _, oID := range sortedUsedOIDs {
		fnName, ok := overloadToFnName[oID]
		if !ok {
			fnName = "UNKNOWN — not in registered functions"
		}
		fmt.Fprintf(os.Stderr, "[cel:probe:ast:oid]   oID=%-42s → fn=%s\n", oID, fnName)
	}

	// ── Step 3: identify which functions are needed ───────────────────────────
	neededFns := make(map[string]struct{}, len(usedOIDs))
	for oID := range usedOIDs {
		if fnName, ok := overloadToFnName[oID]; ok {
			neededFns[fnName] = struct{}{}
		}
	}
	fmt.Fprintf(os.Stderr,
		"[cel:probe:fn:filter] totalFns=%d  totalDeclaredOverloads=%d  neededFns=%d  skippedFns=%d\n",
		len(fns), totalDeclaredOverloads, len(neededFns), len(fns)-len(neededFns))

	// ── Step 4: for each needed function, call Bindings() and add selectively ─
	//
	// dispatcher.Add(b) implementation (from interpreter/dispatcher.go):
	//   func (d *defaultDispatcher) Add(overloads ...*functions.Overload) error {
	//     for _, o := range overloads {
	//       if _, found := d.overloads[o.Operator]; found {
	//         return fmt.Errorf("overload already exists '%s'", o.Operator)
	//       }
	//       d.overloads[o.Operator] = o   // ← this is the ONLY thing Add() does
	//     }
	//     return nil
	//   }
	//
	// So the dispatcher is just map[string]*functions.Overload keyed by b.Operator.
	// The planner calls disp.FindOverload(overloadID) at plan time to bake
	// the function pointer into the interpretable tree node.
	boundTotal := 0
	sortedNeeded := sortedStringSet(neededFns)
	for _, name := range sortedNeeded {
		fn := fns[name]
		overloadDecls := fn.OverloadDecls()
		declIDs := make([]string, len(overloadDecls))
		for i, o := range overloadDecls {
			declIDs[i] = o.ID()
		}

		// fn.Bindings() allocates *functions.Overload structs with guarded closures.
		// Each closure wraps the actual implementation with type-safety checks.
		bindings, err := fn.Bindings()
		if err != nil {
			return err
		}

		// Classify the binding structure (see Cases A–E in the doc comment).
		structure := celBindingStructure(name, overloadDecls, bindings)
		usedInThisFn := make([]string, 0, 2)
		for _, o := range overloadDecls {
			if _, used := usedOIDs[o.ID()]; used {
				usedInThisFn = append(usedInThisFn, o.ID())
			}
		}
		fmt.Fprintf(os.Stderr,
			"[cel:probe:fn:needed] fn=%-35s  declaredOverloads=%d  declIDs=%v  usedInThisFn=%v  bindingsProduced=%d  structure=%s\n",
			name, len(overloadDecls), declIDs, usedInThisFn, len(bindings), structure)

		// plannerBypassOps lists functions that the planner short-circuits in planCall()
		// BEFORE calling disp.FindOverload().  Entries for these operators are added to
		// the dispatcher as a safety net but the planner will NEVER read them.
		plannerBypassOps := map[string]struct{}{
			"_&&_": {}, "_||_": {}, "_?_:_": {},
			"_==_": {}, "_!=_": {},
			"_[_]": {}, "_?._": {}, "_[?_]": {},
		}

		// Inspect and selectively add each binding entry.
		// Rules:
		//   fn-name entry (b.Operator == name): always add — required as fallback dispatch path
		//     unless the fn is a planner-bypassed operator (safe to add anyway).
		//   per-overload entry (b.Operator == overloadID): add only if in usedOIDs.
		for _, b := range bindings {
			isFnNameEntry := b.Operator == name
			implDesc := celOverloadImplDesc(b) // "unary" / "binary" / "function" / "nil"

			_, isBypassed := plannerBypassOps[b.Operator]

			var action string
			var shouldAdd bool
			if isFnNameEntry {
				// The function-name entry handles runtime dispatch for three cases:
				//  - Case A: singleton binding (this IS the only implementation)
				//  - Case B: overload whose ID equals fnName (effectively the same)
				//  - Case C fallback: copy of the single overload for fn-name lookup
				//  - Case D dispatcher: funcDispatch closure that tries each overload by type
				// Always register it so the planner fallback path FindOverload(fnName) works.
				if isBypassed {
					action = "ADD  (fn-name key — planner BYPASSES dispatcher for this op, added as safety net only)"
				} else {
					action = "ADD  (fn-name key — planner PATH-2 fallback: FindOverload(fnName) if overload-ID misses)"
				}
				shouldAdd = true
			} else if _, used := usedOIDs[b.Operator]; used {
				// Per-overload entry whose ID was resolved by the type-checker.
				// This is the PRIMARY planner PATH-1: FindOverload(overloadID).
				action = "ADD  (overload-ID key — planner PATH-1 direct: FindOverload(overloadID))"
				shouldAdd = true
			} else {
				// Per-overload entry for an overload this expression does not use.
				// Skipping saves one *functions.Overload closure (~320 B).
				action = "SKIP (overload-ID key — not referenced by type-checker, not needed)"
				shouldAdd = false
			}

			fmt.Fprintf(os.Stderr,
				"[cel:probe:binding:entry]   dispatchKey=%-44s  impl=%-8s  trait=%d  nonStrict=%-5v  → %s\n",
				b.Operator, implDesc, b.OperandTrait, b.NonStrict, action)

			if shouldAdd {
				if err = disp.Add(b); err != nil {
					return err
				}
				boundTotal++
			}
		}
	}

	// ── Step 5: dispatch resolution verification ─────────────────────────────
	// Verify every used OID will resolve at plan time.  There are THREE valid
	// resolution paths — a miss on path 1 is NOT necessarily an error:
	//
	// Path 1 (direct):    disp.FindOverload(overloadID) — used when fn.Bindings()
	//   produced an entry keyed by the specific overload ID (Case C primary key).
	//
	// Path 2 (fallback):  disp.FindOverload(fnName) — used when Bindings() only
	//   produced an fn-name entry (singleton, or multi-overload dispatcher entry).
	//   From planner.go: "if fnDef==nil { fnDef,_ = p.disp.FindOverload(fnName) }"
	//
	// Path 3 (bypassed):  the planner special-cases 8 operators BEFORE calling
	//   FindOverload at all — their dispatcher entries are NEVER looked up:
	//     _&&_  _||_  _?_:_  _==_  _!=_  _[_]  _?._  _[?_]
	//   (from planner.go planCall() switch block, lines 244-258)
	//   We still add them above as a safety net but they are never consumed.
	plannerBypassedOps := map[string]struct{}{
		"_&&_": {}, "_||_": {}, "_?_:_": {},
		"_==_": {}, "_!=_": {},
		"_[_]": {}, "_?._": {}, "_[?_]": {},
	}

	fmt.Fprintf(os.Stderr,
		"[cel:probe:dispatch:verify] verifying %d used OIDs across 3 resolution paths (dispatcherSize=%d):\n",
		len(usedOIDs), len(disp.OverloadIds()))
	misses := 0
	for _, oID := range sortedUsedOIDs {
		fnName := overloadToFnName[oID]
		_, directFound := disp.FindOverload(oID)
		_, fallbackFound := disp.FindOverload(fnName)
		_, isBypassed := plannerBypassedOps[fnName]

		switch {
		case directFound:
			fmt.Fprintf(os.Stderr,
				"[cel:probe:dispatch:check]   oID=%-42s → PATH-1 DIRECT  (overload-ID key in dispatcher)\n", oID)
		case isBypassed:
			// Planner switch-cases this fnName before disp.FindOverload is ever called.
			// The dispatcher entry (if any) is added as a safety net but is NEVER read.
			fmt.Fprintf(os.Stderr,
				"[cel:probe:dispatch:check]   oID=%-42s → PATH-3 BYPASSED fn=%s (planner special-cases this op, dispatcher not consulted)\n",
				oID, fnName)
		case fallbackFound:
			fmt.Fprintf(os.Stderr,
				"[cel:probe:dispatch:check]   oID=%-42s → PATH-2 FALLBACK fn=%s (singleton/dominant-overload binding at fn-name key)\n",
				oID, fnName)
		default:
			fmt.Fprintf(os.Stderr,
				"[cel:probe:dispatch:check]   oID=%-42s → *** MISSING — fn=%s: neither overload-ID key, fn-name fallback, nor bypassed operator ***\n",
				oID, fnName)
			misses++
		}
	}
	if misses > 0 {
		fmt.Fprintf(os.Stderr,
			"[cel:probe:dispatch:verify] WARNING: %d/%d OIDs have no valid resolution path — runtime noSuchOverload risk\n",
			misses, len(usedOIDs))
	} else {
		fmt.Fprintf(os.Stderr,
			"[cel:probe:dispatch:verify] OK: all %d OIDs resolve via one of the 3 paths\n", len(usedOIDs))
	}

	// ── Summary ───────────────────────────────────────────────────────────────
	savedB := (totalDeclaredOverloads - boundTotal) * 320
	fmt.Fprintf(os.Stderr,
		"[cel:probe:addNeededBindings] SUMMARY: totalFns=%d  totalDeclaredOverloads=%d  usedOIDs=%d  neededFns=%d  boundEntries=%d  savedFns=%d  savedB≈%d\n",
		len(fns), totalDeclaredOverloads, len(usedOIDs), len(neededFns), boundTotal, len(fns)-len(neededFns), savedB)
	return nil
}

// sortedStringSet returns a sorted slice of keys from a map[string]struct{}.
func sortedStringSet(m map[string]struct{}) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

// celBindingStructure classifies which Bindings() case applies, inferring it
// from the declared overload count, the produced binding count, and the binding key.
//
// Background — what fn.Bindings() produces (from decls/decls.go):
//
//	Case A  singleton (f.singleton!=nil):
//	   0 declared-overload entries have bindings; f.singleton provides 1 entry
//	   keyed by fnName.  Note: declared overloads MAY still exist as type-checker
//	   declarations even when the singleton handles all runtime dispatch.
//	   → bindings=1, bindings[0].Operator==fnName
//
//	Case B  single overload, overloadID == fnName:
//	   1 declared overload with hasBinding()=true AND its ID == fnName.
//	   Only 1 entry is produced (no fallback needed since key already is fnName).
//	   → bindings=1, bindings[0].Operator==fnName, overloadDecls[0].ID()==fnName
//
//	Case C  single overload, overloadID != fnName:
//	   1 declared overload with hasBinding()=true AND its ID != fnName.
//	   2 entries: the overload-ID entry (primary) + a copy keyed by fnName (fallback).
//	   → bindings=2, bindings[0].Operator==overloadID, bindings[1].Operator==fnName
//
//	Case D  multiple overloads, at least 2 have bindings:
//	   N per-overload entries (keyed by overload ID) + 1 fnName funcDispatch entry.
//	   → bindings=N+1, last entry's Operator==fnName
//
//	Case E  no binding (declaration-only):
//	   → bindings=0
//
// A/B ambiguity: Cases A and B both produce 1 binding keyed by fnName.
// We can distinguish them when overloadDecls has 1 entry whose ID == fnName (→ B),
// or when overloadDecls is empty OR the sole declared overload's ID != fnName (→ A).
func celBindingStructure(fnName string, overloadDecls []*decls.OverloadDecl, bindings []*functions.Overload) string {
	switch {
	case len(bindings) == 0:
		return "Case E: no-binding (declaration-only, no implementation registered)"

	case len(bindings) == 1 && bindings[0].Operator == fnName:
		// Cases A or B: only a fn-name-keyed entry was produced.
		if len(overloadDecls) == 1 && overloadDecls[0].ID() == fnName {
			// Case B: the single overload's ID equals the fn name; it IS the binding.
			return "Case B: 1 entry (overloadID==fnName — the overload itself IS the fn-name key)"
		}
		if len(overloadDecls) == 0 {
			// Case A (pure singleton, no declared overloads).
			return "Case A: 1 entry (singleton via SingletonXxxBinding, no declared overloads)"
		}
		// Case A variant: declared overloads exist BUT none has hasBinding()=true;
		// the singleton provides the single fn-name binding.
		// This is what _&&_, _[_], and multi-overload-with-singleton functions look like.
		declIDs := make([]string, len(overloadDecls))
		for i, o := range overloadDecls {
			declIDs[i] = o.ID()
		}
		return fmt.Sprintf(
			"Case A (singleton+declarations): 1 fn-name entry; %d declared overloads %v have no per-overload binding",
			len(overloadDecls), declIDs)

	case len(bindings) == 1 && bindings[0].Operator != fnName:
		// Unusual: single entry but it's NOT the fn-name key. Shouldn't occur in practice.
		return fmt.Sprintf("Case ?: 1 entry keyed by %q (not fn-name %q) — unexpected", bindings[0].Operator, fnName)

	case len(bindings) == 2 && len(overloadDecls) == 1:
		// Case C: the single declared overload (overloadID != fnName) + fn-name fallback copy.
		return fmt.Sprintf(
			"Case C: 2 entries — overload-ID key %q (primary) + fn-name key %q (fallback copy)",
			bindings[0].Operator, fnName)

	default:
		// Case D: N per-overload entries (keyed by overload IDs) + 1 fnName funcDispatch entry.
		return fmt.Sprintf(
			"Case D: %d entries = %d overload-ID keys + 1 fn-name funcDispatch entry (multi-overload dynamic dispatch)",
			len(bindings), len(bindings)-1)
	}
}

// celOverloadImplDesc returns a short label for which implementation field is
// set on a *functions.Overload, showing what the dispatcher will call.
//
//   unary     → guardedUnaryOp closure (1-arg, type-checked)
//   binary    → guardedBinaryOp closure (2-arg, type-checked)
//   function  → guardedFunctionOp OR multi-dispatch funcDispatch closure
//   nil       → no implementation (placeholder / declaration-only)
func celOverloadImplDesc(b *functions.Overload) string {
	hasUnary := b.Unary != nil
	hasBinary := b.Binary != nil
	hasFn := b.Function != nil
	switch {
	case hasUnary && !hasBinary && !hasFn:
		return "unary"
	case hasBinary && !hasUnary && !hasFn:
		return "binary"
	case hasFn && !hasUnary && !hasBinary:
		return "function"
	case hasUnary || hasBinary || hasFn:
		return "mixed"
	default:
		return "nil"
	}
}

// max returns the larger of two ints (Go 1.21+ builtin, provided here for older toolchains).
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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
