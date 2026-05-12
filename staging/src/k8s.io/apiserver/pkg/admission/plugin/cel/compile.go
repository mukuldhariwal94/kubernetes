/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cel

import (
	"fmt"
	"hash/fnv"
	"runtime"
	"sort"
	"sync"
	"time"
	"unsafe"

	"github.com/google/cel-go/cel"

	"k8s.io/apimachinery/pkg/util/version"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/common"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/apiserver/pkg/cel/library"
	"k8s.io/apiserver/pkg/cel/mutation"
	"k8s.io/klog/v2"
)

const (
	ObjectVarName                    = "object"
	OldObjectVarName                 = "oldObject"
	ParamsVarName                    = "params"
	RequestVarName                   = "request"
	NamespaceVarName                 = "namespaceObject"
	AuthorizerVarName                = "authorizer"
	RequestResourceAuthorizerVarName = "authorizer.requestResource"
	VariableVarName                  = "variables"
)

// BuildRequestType generates a DeclType for AdmissionRequest. This may be replaced with a utility that
// converts the native type definition to apiservercel.DeclType once such a utility becomes available.
// The 'uid' field is omitted since it is not needed for in-process admission review.
// The 'object' and 'oldObject' fields are omitted since they are exposed as root level CEL variables.
func BuildRequestType() *apiservercel.DeclType {
	field := func(name string, declType *apiservercel.DeclType, required bool) *apiservercel.DeclField {
		return apiservercel.NewDeclField(name, declType, required, nil, nil)
	}
	fields := func(fields ...*apiservercel.DeclField) map[string]*apiservercel.DeclField {
		result := make(map[string]*apiservercel.DeclField, len(fields))
		for _, f := range fields {
			result[f.Name] = f
		}
		return result
	}
	gvkType := apiservercel.NewObjectType("kubernetes.GroupVersionKind", fields(
		field("group", apiservercel.StringType, true),
		field("version", apiservercel.StringType, true),
		field("kind", apiservercel.StringType, true),
	))
	gvrType := apiservercel.NewObjectType("kubernetes.GroupVersionResource", fields(
		field("group", apiservercel.StringType, true),
		field("version", apiservercel.StringType, true),
		field("resource", apiservercel.StringType, true),
	))
	userInfoType := apiservercel.NewObjectType("kubernetes.UserInfo", fields(
		field("username", apiservercel.StringType, false),
		field("uid", apiservercel.StringType, false),
		field("groups", apiservercel.NewListType(apiservercel.StringType, -1), false),
		field("extra", apiservercel.NewMapType(apiservercel.StringType, apiservercel.NewListType(apiservercel.StringType, -1), -1), false),
	))
	return apiservercel.NewObjectType("kubernetes.AdmissionRequest", fields(
		field("kind", gvkType, true),
		field("resource", gvrType, true),
		field("subResource", apiservercel.StringType, false),
		field("requestKind", gvkType, true),
		field("requestResource", gvrType, true),
		field("requestSubResource", apiservercel.StringType, false),
		field("name", apiservercel.StringType, true),
		field("namespace", apiservercel.StringType, false),
		field("operation", apiservercel.StringType, true),
		field("userInfo", userInfoType, true),
		field("dryRun", apiservercel.BoolType, false),
		field("options", apiservercel.DynType, false),
	))
}

// BuildNamespaceType generates a DeclType for Namespace.
// Certain nested fields in Namespace (e.g. managedFields, ownerReferences etc.) are omitted in the generated DeclType
// by design.
func BuildNamespaceType() *apiservercel.DeclType {
	field := func(name string, declType *apiservercel.DeclType, required bool) *apiservercel.DeclField {
		return apiservercel.NewDeclField(name, declType, required, nil, nil)
	}
	fields := func(fields ...*apiservercel.DeclField) map[string]*apiservercel.DeclField {
		result := make(map[string]*apiservercel.DeclField, len(fields))
		for _, f := range fields {
			result[f.Name] = f
		}
		return result
	}

	specType := apiservercel.NewObjectType("kubernetes.NamespaceSpec", fields(
		field("finalizers", apiservercel.NewListType(apiservercel.StringType, -1), true),
	))
	conditionType := apiservercel.NewObjectType("kubernetes.NamespaceCondition", fields(
		field("status", apiservercel.StringType, true),
		field("type", apiservercel.StringType, true),
		field("lastTransitionTime", apiservercel.TimestampType, true),
		field("message", apiservercel.StringType, true),
		field("reason", apiservercel.StringType, true),
	))
	statusType := apiservercel.NewObjectType("kubernetes.NamespaceStatus", fields(
		field("conditions", apiservercel.NewListType(conditionType, -1), true),
		field("phase", apiservercel.StringType, true),
	))
	metadataType := apiservercel.NewObjectType("kubernetes.NamespaceMetadata", fields(
		field("name", apiservercel.StringType, true),
		field("generateName", apiservercel.StringType, true),
		field("namespace", apiservercel.StringType, true),
		field("labels", apiservercel.NewMapType(apiservercel.StringType, apiservercel.StringType, -1), true),
		field("annotations", apiservercel.NewMapType(apiservercel.StringType, apiservercel.StringType, -1), true),
		field("UID", apiservercel.StringType, true),
		field("creationTimestamp", apiservercel.TimestampType, true),
		field("deletionGracePeriodSeconds", apiservercel.IntType, true),
		field("deletionTimestamp", apiservercel.TimestampType, true),
		field("generation", apiservercel.IntType, true),
		field("resourceVersion", apiservercel.StringType, true),
		field("finalizers", apiservercel.NewListType(apiservercel.StringType, -1), true),
	))
	return apiservercel.NewObjectType("kubernetes.Namespace", fields(
		field("metadata", metadataType, true),
		field("spec", specType, true),
		field("status", statusType, true),
	))
}

// CompilationResult represents a compiled validations expression.
type CompilationResult struct {
	Program            cel.Program
	Error              *apiservercel.Error
	ExpressionAccessor ExpressionAccessor
	OutputType         *cel.Type
}

// Compiler provides a CEL expression compiler configured with the desired admission related CEL variables and
// environment mode.
type Compiler interface {
	CompileCELExpression(expressionAccessor ExpressionAccessor, options OptionalVariableDeclarations, mode environment.Type) CompilationResult
}

type compiler struct {
	varEnvs variableDeclEnvs
}

func NewCompiler(env *environment.EnvSet) Compiler {
	return &compiler{varEnvs: mustBuildEnvs(env)}
}

type variableDeclEnvs map[OptionalVariableDeclarations]*environment.EnvSet

// CompileCELExpression returns a compiled CEL expression with full per-phase memory and timing
// instrumentation.
//
// Instrumentation layers (all logged via klog.InfoS):
//  1. runtime.ReadMemStats snapshots before/after each phase — captures heap delta and malloc count
//  2. Per-phase timing via time.Now()
//  3. AST structure analysis — reference map node count and output type from type-checker
//  4. GC stats (NumGC, PauseTotalNs) to detect compilation-driven GC pressure
//  5. Per-expression FNV-32a hash for cross-line log correlation
//
// Log lines emitted per compilation:
//   - cel.env.configured   (once per env variant, at mustBuildEnvs time)
//   - cel.compile          (after env.Compile — AST phase)
//   - cel.overload.analysis (binding waste analysis)
//   - cel.program          (after env.Program — dispatcher + interpretable phase)
//   - cel.expression.summary (end-to-end totals + retained-memory estimate)
func (c compiler) CompileCELExpression(expressionAccessor ExpressionAccessor, options OptionalVariableDeclarations, envType environment.Type) CompilationResult {
	resultError := func(errorString string, errType apiservercel.ErrorType, cause error) CompilationResult {
		return CompilationResult{
			Error: &apiservercel.Error{
				Type:   errType,
				Detail: errorString,
				Cause:  cause,
			},
			ExpressionAccessor: expressionAccessor,
		}
	}

	expr := expressionAccessor.GetExpression()
	hash := celExprHash(expr)
	t0 := time.Now()

	// Snapshot heap state before any work so we can compute total allocated bytes.
	var ms0, ms1, ms2 runtime.MemStats
	runtime.ReadMemStats(&ms0)

	// ── Phase 0: environment lookup ─────────────────────────────────────────
	env, err := c.varEnvs[options].Env(envType)
	if err != nil {
		return resultError(fmt.Sprintf("unexpected error loading CEL environment: %v", err), apiservercel.ErrorTypeInternal, nil)
	}

	// ── Phase 1: env.Compile ────────────────────────────────────────────────
	// Parses the expression text, resolves identifiers against the env's
	// declaration set, type-checks each sub-expression, and builds the typed
	// AST (celast.AST) with a ReferenceMap and TypeMap.
	//
	// Allocates (per expression):
	//   - parser token list → []exprpb.Expr nodes (one per token/operator)
	//   - type-checker typeMap: map[int64]ref.Type
	//   - type-checker refMap:  map[int64]*exprpb.Reference
	//   - source info: character offsets and line positions
	//
	// Memory scales as O(tokens) — longer/more complex expressions cost more.
	tCompile := time.Now()
	ast, issues := env.Compile(expr)
	tCompileElapsed := time.Since(tCompile)
	runtime.ReadMemStats(&ms1)

	if issues != nil {
		return resultError("compilation failed: "+issues.String(), apiservercel.ErrorTypeInvalid, apiservercel.NewCompilationError(issues))
	}
	found := false
	returnTypes := expressionAccessor.ReturnTypes()
	for _, returnType := range returnTypes {
		if ast.OutputType().IsExactType(returnType) || cel.AnyType.IsExactType(returnType) {
			found = true
			break
		}
	}
	if !found {
		var reason string
		if len(returnTypes) == 1 {
			reason = fmt.Sprintf("must evaluate to %v but got %v", returnTypes[0].String(), ast.OutputType().String())
		} else {
			reason = fmt.Sprintf("must evaluate to one of %v but got %v", returnTypes, ast.OutputType().String())
		}
		return resultError(reason, apiservercel.ErrorTypeInvalid, nil)
	}

	_, err = cel.AstToCheckedExpr(ast)
	if err != nil {
		// should be impossible since env.Compile returned no issues
		return resultError("unexpected compilation error: "+err.Error(), apiservercel.ErrorTypeInternal, nil)
	}

	logCELCompilePhase(hash, expr, ast, tCompileElapsed, &ms0, &ms1)
	// logFunctionBindingAnalysis returns the number of distinct overload IDs
	// actually resolved by the type-checker for this expression. This is the
	// minimum number of dispatcher entries that addNeededBindings would bind.
	usedOIDCount := logFunctionBindingAnalysis(env, ast, expr)

	// ── Phase 2: env.Program ────────────────────────────────────────────────
	// Builds the dispatcher (map[string]*functions.Overload, one per bound
	// overload), creates the interpreter, and runs the planner which walks
	// the typed AST to produce the interpretable eval tree.
	//
	// Allocates PER PROGRAM (not shared across programs from the same env):
	//   1. *prog struct               ~192 B header
	//   2. dispatcher map             new map[string]*functions.Overload
	//   3. *functions.Overload × N    each holds guard-closure wrappers
	//      Without addNeededBindings: N ≈ 352 (all fns)
	//      With    addNeededBindings: N ≈ usedOIDCount × 2 (~10–30)
	//   4. interpreter                small struct; holds POINTERS to shared *Env fields
	//   5. interpretable tree         planner output; proportional to AST node count
	//
	// NOT allocated per program (shared via the embedded *Env pointer):
	//   - variables, functions map, macros, adapter, provider, checker.Env, parser
	tProgram := time.Now()
	prog, err := env.Program(ast,
		cel.InterruptCheckFrequency(celconfig.CheckFrequency),
	)
	tProgramElapsed := time.Since(tProgram)
	runtime.ReadMemStats(&ms2)

	if err != nil {
		return resultError("program instantiation failed: "+err.Error(), apiservercel.ErrorTypeInternal, nil)
	}

	// Capture the program-phase heap delta before calling summary so it can be
	// reported as the "measured" value alongside the formula-based estimate.
	programHeapDeltaB, _ := celMemDelta(&ms1, &ms2)

	logCELProgramPhase(hash, prog, tProgramElapsed, &ms1, &ms2)
	logCELExpressionSummary(hash, expr, ast, env, prog, usedOIDCount, programHeapDeltaB, &ms0, &ms2, time.Since(t0))

	return CompilationResult{
		Program:            prog,
		ExpressionAccessor: expressionAccessor,
		OutputType:         ast.OutputType(),
	}
}

func mustBuildEnvs(baseEnv *environment.EnvSet) variableDeclEnvs {
	requestType := BuildRequestType()
	namespaceType := BuildNamespaceType()
	envs := make(variableDeclEnvs, 8) // since the number of variable combinations is small, pre-build a environment for each
	for _, hasParams := range []bool{false, true} {
		for _, hasAuthorizer := range []bool{false, true} {
			var err error
			{
				decl := OptionalVariableDeclarations{HasParams: hasParams, HasAuthorizer: hasAuthorizer}
				envs[decl], err = createEnvForOpts(baseEnv, namespaceType, requestType, decl)
				if err != nil {
					panic(err)
				}
				logEnvConfiguration(envs[decl], decl)
			}
			{
				decl := OptionalVariableDeclarations{HasParams: hasParams, HasAuthorizer: hasAuthorizer, HasPatchTypes: true}
				envs[decl], err = createEnvForOpts(baseEnv, namespaceType, requestType, decl)
				if err != nil {
					panic(err)
				}
				logEnvConfiguration(envs[decl], decl)
			}
		}
	}
	return envs
}

func createEnvForOpts(baseEnv *environment.EnvSet, namespaceType *apiservercel.DeclType, requestType *apiservercel.DeclType, opts OptionalVariableDeclarations) (*environment.EnvSet, error) {
	var envOpts []cel.EnvOption
	envOpts = append(envOpts,
		cel.Variable(ObjectVarName, cel.DynType),
		cel.Variable(OldObjectVarName, cel.DynType),
		cel.Variable(NamespaceVarName, namespaceType.CelType()),
		cel.Variable(RequestVarName, requestType.CelType()))
	if opts.HasParams {
		envOpts = append(envOpts, cel.Variable(ParamsVarName, cel.DynType))
	}
	if opts.HasAuthorizer {
		envOpts = append(envOpts,
			cel.Variable(AuthorizerVarName, library.AuthorizerType),
			cel.Variable(RequestResourceAuthorizerVarName, library.ResourceCheckType))
	}

	extended, err := baseEnv.Extend(
		environment.VersionedOptions{
			// Feature epoch was actually 1.26, but we artificially set it to 1.0 because these
			// options should always be present.
			IntroducedVersion: version.MajorMinor(1, 0),
			EnvOptions:        envOpts,
			DeclTypes: []*apiservercel.DeclType{
				namespaceType,
				requestType,
			},
		},
		environment.StrictCostOpt,
	)
	if err != nil {
		return nil, fmt.Errorf("environment misconfigured: %w", err)
	}

	if opts.HasPatchTypes {
		extended, err = extended.Extend(hasPatchTypes)
		if err != nil {
			return nil, fmt.Errorf("environment misconfigured: %w", err)
		}
	}
	return extended, nil
}

var hasPatchTypes = environment.VersionedOptions{
	// Feature epoch was actually 1.32, but we artificially set it to 1.0 because these
	// options should always be present.
	IntroducedVersion: version.MajorMinor(1, 0),
	EnvOptions: []cel.EnvOption{
		common.ResolverEnvOption(&mutation.DynamicTypeResolver{}),
		environment.UnversionedLib(library.JSONPatch), // for jsonPatch.escape() function
	},
}

// ── Instrumentation helpers ────────────────────────────────────────────────

// celExprHash returns an 8-hex-character FNV-32a hash of the expression text.
// Used to correlate multi-line log output for the same expression across phases.
func celExprHash(expr string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(expr))
	return fmt.Sprintf("%08x", h.Sum32())
}

// celTruncate truncates s to at most n runes, appending "…" if truncated.
func celTruncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// celMemDelta returns the signed heap delta and unsigned malloc count delta
// between two ReadMemStats snapshots.
//
// Note: runtime.ReadMemStats acquires the global heap lock (brief STW in older
// Go; lock-based in Go ≥1.21). Acceptable for debug instrumentation; remove
// the ReadMemStats calls in production once investigation is complete.
func celMemDelta(before, after *runtime.MemStats) (heapDeltaB int64, mallocsDelta uint64) {
	heapDeltaB = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	mallocsDelta = after.Mallocs - before.Mallocs
	return
}

// logCELCompilePhase logs the result of the env.Compile phase (parse + type-check).
//
// Key fields:
//   - astRefNodes   number of resolved call/identifier references in the typed AST
//     (proxy for expression complexity; scales with operator/function count)
//   - heapDeltaB    bytes allocated on the heap during this phase
//   - mallocs       number of heap allocations (each alloc is a separate object)
//   - elapsedUs     wall-clock microseconds
//
// What this phase allocates:
//   - Parser output: one exprpb.Expr node per token/operator
//   - Type-checker typeMap and refMap (both map[int64]…)
//   - Source info: character offsets and newline positions
//   - The *cel.Ast wrapper struct
func logCELCompilePhase(hash, expr string, ast *cel.Ast, elapsed time.Duration, ms0, ms1 *runtime.MemStats) {
	refMap := ast.NativeRep().ReferenceMap()
	heapDelta, mallocs := celMemDelta(ms0, ms1)

	klog.InfoS("cel.compile",
		"exprHash", hash,
		"expr", celTruncate(expr, 120),
		"astRefNodes", len(refMap),
		"astOutputType", ast.OutputType().String(),
		"astIsChecked", ast.IsChecked(),
		"elapsedUs", elapsed.Microseconds(),
		"heapDeltaB", heapDelta,
		"mallocs", mallocs,
		"numGC", ms1.NumGC,
		"gcPauseTotalNs", ms1.PauseTotalNs,
	)
}

// logCELProgramPhase logs the result of the env.Program phase (dispatcher + planner).
//
// Key fields:
//   - progPtr       pointer address of the compiled program — compare across compilations
//     to confirm each call produces a distinct object (no accidental sharing)
//   - heapDeltaB    bytes allocated during program construction
//     with addNeededBindings: ~2–5 KB; without: ~60–90 KB
//   - mallocs       number of allocations; dominated by *functions.Overload closures
//
// What this phase allocates (per program, not shared):
//  1. *prog struct header             ~192 B
//  2. dispatcher map                  new map[string]*functions.Overload per call
//  3. *functions.Overload × N closures  N = bound overloads × ~256 B each
//  4. interpreter                     small struct, holds shared *Env field pointers
//  5. interpretable eval tree         planner output, ~120 B × AST node count
//
// What is NOT allocated here (shared via embedded *Env pointer):
//
//	variables, functions, macros, adapter, provider, checker.Env, parser
func logCELProgramPhase(hash string, prog cel.Program, elapsed time.Duration, ms1, ms2 *runtime.MemStats) {
	heapDelta, mallocs := celMemDelta(ms1, ms2)

	klog.InfoS("cel.program",
		"exprHash", hash,
		"progPtr", fmt.Sprintf("%p", prog),
		"elapsedUs", elapsed.Microseconds(),
		"heapDeltaB", heapDelta,
		"mallocs", mallocs,
	)
}

// logCELExpressionSummary logs the end-to-end summary for one expression compilation.
//
// Why the previous estimatedRetainedB=3224 was WRONG
//
// The old formula used astRefNodes (e.g. 8) as a proxy for dispatcher entries.
// But fn.Bindings() is called for every registered function regardless of the
// expression — producing ~352 entries in the dispatcher, not 8.
// The correct pre-patch formula must use totalEnvOverloads as the dispatcher size.
//
// Per-entry cost breakdown (per *functions.Overload in the dispatcher):
//
//	*functions.Overload struct:  56 B  (Operator string + 3 func ptrs + trait + bool)
//	guard closure heap objects: ~200 B  (guardedUnaryOp/Binary/FunctionOp closures)
//	map bucket overhead:         ~72 B  (amortised map[string]* bucket cost)
//	total per entry:            ~328 B  (use 320 B for clean math)
//
// Dispatcher entry count:
//
//	345 declared overloads → ~352 actual entries because fn.Bindings() adds a
//	SECOND entry keyed by function name for single-overload functions where the
//	overload ID differs from the function name (measured: +7 extras = +2%).
//
// Corrected estimates:
//
//	CURRENT (unpatched):  240 + 352×320 + astNodes×120 ≈ 113 KB  (matches measured ~138 KB)
//	POST-PATCH:           240 + usedOIDs×2×320 + astNodes×120 ≈ 3–10 KB
//	Savings at 25k progs: 25000 × (113-5) KB ≈ 2.7 GB → 125 MB
func logCELExpressionSummary(hash, expr string, ast *cel.Ast, env *cel.Env, prog cel.Program,
	usedOIDCount int, measuredProgramHeapB int64, ms0, ms2 *runtime.MemStats, totalElapsed time.Duration) {

	refMap := ast.NativeRep().ReferenceMap()
	heapDelta, totalMallocs := celMemDelta(ms0, ms2)

	// Compute actual env overload count to derive accurate dispatcher size.
	totalEnvOverloads := 0
	for _, fn := range env.Functions() {
		totalEnvOverloads += len(fn.OverloadDecls())
	}
	// fn.Bindings() produces ~2% extra entries for singleton functions (measured: 345→352).
	totalBindingEntries := int64(totalEnvOverloads) * 1021 / 1000

	// Per-entry cost: Overload struct + guard closures + map bucket ≈ 320 B.
	const perEntryB = int64(320)

	// CURRENT (unpatched): all ~352 entries allocated per program.
	currentDispatcherB := totalBindingEntries * perEntryB
	interpretableB := int64(len(refMap)) * 120
	currentEstimatedRetainedB := int64(240) + currentDispatcherB + interpretableB

	// POST-PATCH (with addNeededBindings): only usedOIDCount overloads bound.
	// Each used overload ID gets one entry; its function also gets a singleton
	// fallback entry keyed by function name → approximately ×2 entries total.
	patchedDispatcherB := int64(usedOIDCount) * 2 * perEntryB
	patchedEstimatedRetainedB := int64(240) + patchedDispatcherB + interpretableB

	estimatedSavingB := currentEstimatedRetainedB - patchedEstimatedRetainedB
	wastePct := 0
	if totalEnvOverloads > 0 {
		wastePct = (totalEnvOverloads - usedOIDCount) * 100 / totalEnvOverloads
	}

	klog.InfoS("cel.expression.summary",
		"exprHash", hash,
		"expr", celTruncate(expr, 80),
		"progPtr", fmt.Sprintf("%p", prog),
		"astRefNodes", len(refMap),
		"astOutputType", ast.OutputType().String(),
		// ── Timing ─────────────────────────────────────────────────────────
		"totalElapsedUs", totalElapsed.Microseconds(),
		// ── Measured memory (actual runtime.ReadMemStats deltas) ────────────
		// measuredProgramHeapB: net live-heap increase during env.Program() only.
		// totalHeapDeltaB: net increase across the full compile+program phases.
		// Note: HeapAlloc is live objects; GC between snapshots can make these
		// smaller than gross allocations. See totalMallocs for gross alloc count.
		"measuredProgramHeapB", measuredProgramHeapB,
		"totalHeapDeltaB", heapDelta,
		"totalMallocs", totalMallocs,
		// ── Formula-based estimates ─────────────────────────────────────────
		// These use the known structure sizes and actual env overload count.
		// currentEstimatedRetainedB: what the program retains WITHOUT the patch.
		// patchedEstimatedRetainedB: what it would retain WITH addNeededBindings.
		"totalEnvOverloads", totalEnvOverloads,
		"dispatcherEntriesEst", totalBindingEntries,
		"usedOIDCount", usedOIDCount,
		"wastePct", wastePct,
		"currentEstimatedRetainedB", currentEstimatedRetainedB,
		"patchedEstimatedRetainedB", patchedEstimatedRetainedB,
		"estimatedSavingB", estimatedSavingB,
		// ── Process-wide heap snapshot ──────────────────────────────────────
		"heapAllocB", ms2.HeapAlloc,
		"heapInuseB", ms2.HeapInuse,
		"numGC", ms2.NumGC,
		"gcPauseTotalMs", ms2.PauseTotalNs/1e6,
	)
}

// logEnvConfiguration logs a one-time summary of a freshly-built CEL environment.
// Fires once per OptionalVariableDeclarations combination (8 total at startup).
//
// This documents the SHARED state that every program compiled from this env
// will reference via its embedded *Env pointer — none of this is duplicated per program.
//
// Memory breakdown of *cel.Env (shared, built once per variant):
//   - functions map: ~157 entries × (*FunctionDecl + overload slice) ≈ 2–4 MB
//   - checker.Env (lazy, built once on first Compile): type-scope chain ≈ 200 KB
//   - parser.Parser: grammar automaton ≈ 20–50 KB
//   - variables slice: N × *VariableDecl (small, ~1 KB)
//   - macros slice: M × Macro interface (small)
//   - libraries map: L × SingletonLibrary interface (small)
func logEnvConfiguration(envSet *environment.EnvSet, opts OptionalVariableDeclarations) {
	env, err := envSet.Env(environment.NewExpressions)
	if err != nil {
		return
	}
	fns := env.Functions()
	totalOverloads := 0
	for _, fn := range fns {
		totalOverloads += len(fn.OverloadDecls())
	}
	vars := env.Variables()
	varNames := make([]string, 0, len(vars))
	for _, v := range vars {
		varNames = append(varNames, v.Name())
	}
	sort.Strings(varNames)
	fnNames := make([]string, 0, len(fns))
	for name := range fns {
		fnNames = append(fnNames, name)
	}
	sort.Strings(fnNames)
	klog.InfoS("cel.env.configured",
		"hasParams", opts.HasParams,
		"hasAuthorizer", opts.HasAuthorizer,
		"hasPatchTypes", opts.HasPatchTypes,
		"functions", len(fns),
		"totalOverloads", totalOverloads,
		"variables", varNames,
		"libraries", env.Libraries(),
		"functionNames", fnNames,
	)
}

// logFunctionBindingAnalysis logs the ratio of overloads declared in the env vs.
// those actually resolved by the type-checker for this specific expression.
//
// Key fields:
//   - envOverloads        total overloads registered across all 157 functions
//   - usedOverloads       overloads the type-checker resolved for this expression
//   - wastedOverloads     = envOverloads - usedOverloads
//   - wastedOverloadEstB  estimated bytes wasted per env.Program() without the patch
//     = wastedOverloads × 256 B
//
// This is the core diagnostic: without the addNeededBindings patch, every
// env.Program() call allocates *functions.Overload closure structs for ALL
// envOverloads regardless of whether the expression uses them.
// With the patch, only usedOverloads closures are allocated.
// logFunctionBindingAnalysis returns the number of distinct overload IDs resolved
// by the type-checker for this expression (usedOIDCount). The caller passes this
// to logCELExpressionSummary to compute the post-patch retained-memory estimate.
func logFunctionBindingAnalysis(env *cel.Env, ast *cel.Ast, expr string) int {
	allFns := env.Functions()
	totalOverloads := 0
	for _, fn := range allFns {
		totalOverloads += len(fn.OverloadDecls())
	}

	usedOverloads := make(map[string]struct{})
	for _, ref := range ast.NativeRep().ReferenceMap() {
		for _, oID := range ref.OverloadIDs {
			usedOverloads[oID] = struct{}{}
		}
	}
	used := make([]string, 0, len(usedOverloads))
	for oID := range usedOverloads {
		used = append(used, oID)
	}
	sort.Strings(used)

	klog.InfoS("cel.overload.analysis",
		"exprHash", celExprHash(expr),
		"expr", celTruncate(expr, 100),
		"envFunctions", len(allFns),
		"envOverloads", totalOverloads,
		"usedOverloads", len(usedOverloads),
		"wastedOverloads", totalOverloads-len(usedOverloads),
		// wastedOverloadEstB: bytes saved per program by addNeededBindings.
		// = wastedOverloads × 320 B (Overload struct + closures + map bucket).
		"wastedOverloadEstB", (totalOverloads-len(usedOverloads))*320,
		"usedOverloadIDs", used,
	)
	return len(usedOverloads)
}

// ── Startup struct-size analysis ───────────────────────────────────────────

// celStructSizesOnce ensures the one-time startup analysis fires exactly once.
var celStructSizesOnce sync.Once

// logCELStructSizes logs the shallow struct sizes and scaling projections for
// key CEL types. Runs once at package init via init().
//
// Memory ownership graph for a single compiled cel.Program:
//
//	SHARED (one copy per env variant — 8 variants pre-built at startup):
//	  *cel.Env                 — the parent environment
//	    .functions map          — 157 *FunctionDecl entries (~2–4 MB total)
//	    .variables []*VarDecl   — N variable declarations
//	    .macros    []Macro      — M macros (interface slice)
//	    .adapter   types.Adapter   — value adapter (shared singleton)
//	    .provider  types.Provider  — type provider (shared singleton)
//	    .chk *checker.Env          — lazy, built once on first Compile (~200 KB)
//	    .prsr *parser.Parser        — grammar automaton (~20–50 KB)
//
//	PER-PROGRAM (allocated fresh by env.Program() for each compiled expression):
//	  *prog struct header      ~192 B  (embeds *Env pointer — NOT a copy)
//	  dispatcher map           new map[string]*functions.Overload
//	  *functions.Overload × N  closure wrappers; N = bound overloads
//	    Each Overload: 56 B struct + ~200 B guard closures ≈ 256 B total
//	  interpreter              small struct; holds pointers into shared *Env
//	  interpretable tree       planner output; ~120 B × AST node count
//
//	SCALING CHARACTERISTIC:
//	  total dispatcher cost = N_programs × N_bound_overloads × 256 B
//	  Without addNeededBindings: N_bound = 345  →  88 KB per program
//	  With    addNeededBindings: N_bound ~= 10  →  2.8 KB per program
//	  At 25,000 programs: unpatched ≈ 2.1 GB; patched ≈ 68 MB (~31× reduction)
func logCELStructSizes() {
	celStructSizesOnce.Do(func() {
		type progProxy struct {
			env           uintptr    // *Env embed
			evalOpts      uint64     // EvalOption
			defaultVars   [2]uintptr // Activation (interface)
			dispatcher    [2]uintptr // Dispatcher (interface)
			interpreter   [2]uintptr // Interpreter (interface)
			checkFreq     uint       // interruptCheckFrequency
			plannerOpts   [3]uintptr // []PlannerOption slice header
			regexOpts     [3]uintptr // []*RegexOptimization slice header
			interpretable [2]uintptr // Interpretable (interface)
			observable    uintptr    // *ObservableInterpretable
			costEstimator [2]uintptr // ActualCostEstimator (interface)
			costOptions   [3]uintptr // []CostTrackerOption slice header
			costLimit     uintptr    // *uint64
		}
		klog.InfoS("cel.struct.sizes",
			"CompilationResult_B", unsafe.Sizeof(CompilationResult{}),
			"progProxy_B", unsafe.Sizeof(progProxy{}),
			// dispatcher internals (interpreter/dispatcher.go):
			//   struct{parent Dispatcher(16B) + overloads map(8B)} = 24 B header
			"dispatcherHeader_B", 24,
			// functions.Overload (common/functions/functions.go):
			//   Operator string(16) + OperandTrait int(8) + Unary/Binary/Function func(8×3)
			//   + NonStrict bool(1) + 7 pad = 56 B struct; guard closures add ~200 B on heap
			"functionsOverloadStruct_B", 56,
			"functionsOverloadTotal_B", 256,
			// Dispatcher cost at scale:
			"dispatchCost_unpatched_KB_perProg", 345*256/1024,
			"dispatchCost_patched_KB_perProg", 10*256/1024,
			// 5000 policies × 5 expressions = 25000 programs:
			"dispatchCostTotal_unpatched_MB", 25000*345*256/1024/1024,
			"dispatchCostTotal_patched_MB", 25000*10*256/1024/1024,
		)
	})
}

func init() {
	logCELStructSizes()
}
