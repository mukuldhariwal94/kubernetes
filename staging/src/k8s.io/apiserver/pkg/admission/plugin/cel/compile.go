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

// CompileCELExpression returns a compiled CEL expression with full per-phase instrumentation.
//
// Instrumentation layers:
//  1. runtime.ReadMemStats snapshots before / after each phase (compile, program build)
//  2. Per-phase timing via time.Now()
//  3. AST structure analysis (node / reference count, output type)
//  4. GC stats (NumGC, PauseTotal) to detect GC pressure from compilation
//  5. Per-expression hash for correlation across log lines
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
	var ms0, ms1, ms2 runtime.MemStats
	runtime.ReadMemStats(&ms0)

	// ── Phase 0: environment lookup ─────────────────────────────────────────
	env, err := c.varEnvs[options].Env(envType)
	if err != nil {
		return resultError(fmt.Sprintf("unexpected error loading CEL environment: %v", err), apiservercel.ErrorTypeInternal, nil)
	}

	// ── Phase 1: env.Compile ────────────────────────────────────────────────
	// Parses the expression, resolves identifiers, type-checks, and builds the
	// typed AST.  Allocates: AST nodes, type-check state, source info.
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
		return resultError("unexpected compilation error: "+err.Error(), apiservercel.ErrorTypeInternal, nil)
	}

	// Log AST analysis between phases so logFunctionBindingAnalysis can use the same AST.
	logFunctionBindingAnalysis(env, ast, expr)
	logCELCompilePhase(hash, expr, ast, tCompileElapsed, &ms0, &ms1)

	// ── Phase 2: env.Program ────────────────────────────────────────────────
	// Builds the dispatcher (map[string]*functions.Overload, one per bound
	// overload), creates the interpreter, and runs the planner which walks
	// the typed AST to build the interpretable eval tree.
	//
	// Allocates (per program):
	//   • dispatcher map  — 1 new map[string]*Overload
	//   • *functions.Overload structs — N per fn.Bindings() call (closures!)
	//   • interpreter — holds pointers to shared *Env fields (Container,
	//     adapter, provider, attrFactory) — NOT a full copy
	//   • interpretable tree — proportional to AST node count
	//   • plannerOptions slice
	tProgram := time.Now()
	prog, err := env.Program(ast,
		cel.InterruptCheckFrequency(celconfig.CheckFrequency),
	)
	tProgramElapsed := time.Since(tProgram)
	runtime.ReadMemStats(&ms2)

	if err != nil {
		return resultError("program instantiation failed: "+err.Error(), apiservercel.ErrorTypeInternal, nil)
	}

	logCELProgramPhase(hash, prog, tProgramElapsed, &ms1, &ms2)
	logCELExpressionSummary(hash, expr, ast, prog, &ms0, &ms2, time.Since(t0))

	return CompilationResult{
		Program:            prog,
		ExpressionAccessor: expressionAccessor,
		OutputType:         ast.OutputType(),
	}
}

func mustBuildEnvs(baseEnv *environment.EnvSet) variableDeclEnvs {
	requestType := BuildRequestType()
	namespaceType := BuildNamespaceType()
	envs := make(variableDeclEnvs, 8)
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
	IntroducedVersion: version.MajorMinor(1, 0),
	EnvOptions: []cel.EnvOption{
		common.ResolverEnvOption(&mutation.DynamicTypeResolver{}),
		environment.UnversionedLib(library.JSONPatch),
	},
}

// ── Instrumentation helpers ────────────────────────────────────────────────

// celExprHash returns an 8-hex-character FNV-32a hash of the expression text.
// Used to correlate multi-line log output for the same expression.
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

// celMemDelta returns the signed delta of HeapAlloc and the unsigned delta of
// Mallocs between two MemStats snapshots. ReadMemStats stops the world briefly;
// gate callers behind a verbosity check when in production.
func celMemDelta(before, after *runtime.MemStats) (heapDeltaB int64, mallocsDelta uint64) {
	heapDeltaB = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	mallocsDelta = after.Mallocs - before.Mallocs
	return
}

// logCELCompilePhase logs the result of the env.Compile phase.
//
// What env.Compile allocates:
//   - The parser builds a []exprpb.Expr tree (one node per token)
//   - The type-checker walks it, building a map[int64]ref.Type (typeMap) and
//     map[int64]*exprpb.Reference (refMap)
//   - Source info (character offsets, newline positions)
//   - The resulting *celast.AST wraps the above
//
// Memory scales roughly as O(tokens) — longer expressions → more allocs.
func logCELCompilePhase(hash, expr string, ast *cel.Ast, elapsed time.Duration, ms0, ms1 *runtime.MemStats) {
	refMap := ast.NativeRep().ReferenceMap()
	heapDelta, mallocs := celMemDelta(ms0, ms1)

	klog.InfoS("cel.compile",
		"exprHash", hash,
		"expr", celTruncate(expr, 120),
		// AST structure
		"astRefNodes", len(refMap),
		"astOutputType", ast.OutputType().String(),
		"astIsChecked", ast.IsChecked(),
		// Phase cost
		"elapsedUs", elapsed.Microseconds(),
		"heapDeltaB", heapDelta,
		"mallocs", mallocs,
		// GC pressure since last GC
		"numGC", ms1.NumGC,
		"gcPauseTotalNs", ms1.PauseTotalNs,
	)
}

// logCELProgramPhase logs the result of the env.Program phase.
//
// What env.Program allocates (PER PROGRAM — not shared):
//
//  1. *prog struct (~10 fields, ~160 B header)
//  2. dispatcher  — a NEW map[string]*functions.Overload, populated by
//     fn.Bindings() for each registered function.
//     Each fn.Bindings() call allocates:
//     - a []* functions.Overload slice
//     - per overload: one new *functions.Overload struct  (64 B)
//     containing guarded closure wrappers around the actual impl.
//     The impl func pointers are SHARED; the guard closures are NEW.
//     - single-overload functions get a SECOND entry keyed by fn name
//     (fallback dispatch path) — so actual dispatcher entries ≈ 2×overloads
//  3. interpreter — small struct, holds POINTERS to shared *Env fields
//     (Container, provider, adapter) — NOT a copy
//  4. interpretable tree — built by the planner walking the typed AST;
//     one node per AST call/attribute/const/fold; proportional to expression
//     complexity; bakes function closure pointers from the dispatcher into
//     each call node
//  5. plannerOptions []PlannerOption slice (decorators)
//
// What is SHARED (not allocated per program):
//   - The *Env itself (embedded pointer, same object for all programs)
//   - variables, functions, macros, adapter, provider, checker state
//   - The underlying impl function closures (Unary/Binary/Function fields)
func logCELProgramPhase(hash string, prog cel.Program, elapsed time.Duration, ms1, ms2 *runtime.MemStats) {
	heapDelta, mallocs := celMemDelta(ms1, ms2)

	klog.InfoS("cel.program",
		"exprHash", hash,
		"progPtr", fmt.Sprintf("%p", prog),
		// Phase cost
		"elapsedUs", elapsed.Microseconds(),
		"heapDeltaB", heapDelta,
		"mallocs", mallocs,
	)
}

// logCELExpressionSummary logs the end-to-end summary for one expression compilation.
//
// Retained memory estimate per compiled program:
//
//	prog header               ~160 B  (fixed)
//	dispatcher map header     ~128 B  (fixed)
//	per bound-overload entry  ~200 B  (map bucket + *Overload struct + guard closures)
//	interpretable tree nodes  ~120 B × AST-ref-count  (call nodes with fn pointers)
//
// With 345 bound overloads (unpatched):
//
//	~160 + ~128 + 345×200 + astNodes×120 = ~69 KB for a trivial expression
//
// With ~10 bound overloads (addNeededBindings patch):
//
//	~160 + ~128 + 10×200 + astNodes×120 ≈ 2.3 KB for a trivial expression
func logCELExpressionSummary(hash, expr string, ast *cel.Ast, prog cel.Program,
	ms0, ms2 *runtime.MemStats, totalElapsed time.Duration) {

	refMap := ast.NativeRep().ReferenceMap()
	heapDelta, totalMallocs := celMemDelta(ms0, ms2)

	// Rough retained-memory estimate based on known struct sizes.
	// Dispatcher entry cost: map overhead (~128 B/bucket amortised) +
	// *functions.Overload (64 B) + guard closure (~64 B) ≈ 200 B per bound overload.
	// We use the AST refMap as a proxy for bound overloads (post-patch).
	dispatcherEntryEstB := int64(len(refMap)) * 200
	interpretableNodeEstB := int64(len(refMap)) * 120
	estimatedRetainedB := int64(160+128) + dispatcherEntryEstB + interpretableNodeEstB

	klog.InfoS("cel.expression.summary",
		"exprHash", hash,
		"expr", celTruncate(expr, 80),
		// Identity
		"progPtr", fmt.Sprintf("%p", prog),
		// Structure
		"astRefNodes", len(refMap),
		"astOutputType", ast.OutputType().String(),
		// End-to-end cost
		"totalElapsedUs", totalElapsed.Microseconds(),
		"totalHeapDeltaB", heapDelta,
		"totalMallocs", totalMallocs,
		// Retained-memory estimate
		"estimatedRetainedB", estimatedRetainedB,
		// GC snapshot at end
		"heapAllocB", ms2.HeapAlloc,
		"heapInuseB", ms2.HeapInuse,
		"heapIdleB", ms2.HeapIdle,
		"numGC", ms2.NumGC,
		"gcPauseTotalMs", ms2.PauseTotalNs/1e6,
		// Scaling note:
		// heapDelta ÷ numExpressions ≈ per-expression marginal cost.
		// As numExpressions → large: retained heap scales as
		//   numPrograms × estimatedRetainedB (dispatcher + interpretable)
		// plus the shared *Env (~3 MB fixed per env variant, 8 variants).
	)
}

// logEnvConfiguration logs a one-time summary of a freshly-built CEL environment.
//
// This fires once per OptionalVariableDeclarations combination (8 total).
// It documents the *shared* state that every program from this env will reference.
//
// Memory ownership:
//   - cel.Env struct header: unsafe.Sizeof(cel.Env{}) bytes
//   - functions map: ~80 entries × (map overhead + *FunctionDecl pointer)
//   - variables slice: N × *VariableDecl
//   - macros slice: M × Macro (interface, ~16 B)
//   - checker.Env (lazy, built once): type-map, ref-map — typically ~200 KB
//   - parser.Parser: grammar state, ~10–50 KB
//
// All of the above is shared across ALL programs compiled from this env.
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
		// Shared state counts
		"functions", len(fns),
		"totalOverloads", totalOverloads,
		"variables", varNames,
		"libraries", env.Libraries(),
		"functionNames", fnNames,
	)
}

// logFunctionBindingAnalysis logs the ratio of overloads declared in the env vs.
// overloads actually resolved by the type-checker for a specific expression.
//
// This is the key diagnostic for the overload-binding waste:
//   - wastedOverloads = env overloads that will NOT be referenced at runtime
//   - Every registered overload causes one *functions.Overload allocation in
//     newProgram() UNLESS the addNeededBindings patch is active.
//   - After the patch: only `usedOverloads` closures are allocated.
func logFunctionBindingAnalysis(env *cel.Env, ast *cel.Ast, expr string) {
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
		// Memory implication:
		// wastedOverloads × ~200 B ≈ bytes wasted per env.Program() call
		// (without addNeededBindings patch).
		"wastedOverloadEstB", (totalOverloads-len(usedOverloads))*200,
		"usedOverloadIDs", used,
	)
}

// ── Startup analysis ───────────────────────────────────────────────────────

// celStructSizesOnce ensures struct size analysis is logged exactly once.
var celStructSizesOnce sync.Once

// logCELStructSizes logs the shallow (header) sizes of key CEL structs.
//
// These are the fixed per-instance overheads before any heap-allocated fields.
// Actual retained memory is larger due to map/slice backing arrays.
//
// Ownership summary for a single compiled cel.Program:
//
//	SHARED (one copy per env variant, typically 8 total):
//	  cel.Env           — variables, functions map, macros, checker, parser
//	  checker.Env       — type map, ref map, decl scope chain (~200 KB typical)
//	  parser.Parser     — grammar automaton (~20 KB)
//	  *decls.FunctionDecl × 157 — function declaration trees (small, ~1 KB each)
//
//	PER-PROGRAM (allocated fresh by newProgram for each env.Program() call):
//	  prog              — the cel.Program implementation struct
//	  dispatcher        — map[string]*functions.Overload
//	  *functions.Overload × N — closure wrappers; N = bound overloads
//	    (each Overload holds Unary/Binary/Function closure — ~3 func pointers)
//	  interpreter       — pointers to shared env fields (NOT a copy)
//	  interpretable     — eval tree proportional to AST complexity
//
//	SCALING:
//	  N programs × M bound-overloads × ~200 B/overload = dispatcher cost
//	  N programs × K AST-nodes × ~120 B/node = interpretable tree cost
//	  Where M = 345 (without patch) or ~10 (with addNeededBindings patch)
func logCELStructSizes() {
	celStructSizesOnce.Do(func() {
		// Proxy sizes for unexported cel-go structs.
		// cel.Ast wraps *source + *celast.AST (both pointers: 16 B header).
		// functions.Overload: Operator string(16) + trait int(8) + 3 func ptrs(24) + NonStrict bool(1) = ~56 B
		type progProxy struct {
			// mirrors the exported fields of cel.prog to get a size estimate
			env           uintptr // *Env embed
			evalOpts      uint64
			defaultVars   [2]uintptr // interface
			dispatcher    [2]uintptr // interface
			interpreter   [2]uintptr // interface
			checkFreq     uint
			plannerOpts   [3]uintptr // slice header
			regexOpts     [3]uintptr // slice header
			interpretable [2]uintptr // interface
			observable    uintptr    // pointer
			costEstimator [2]uintptr // interface
			costOptions   [3]uintptr // slice header
			costLimit     uintptr    // *uint64
		}
		klog.InfoS("cel.struct.sizes",
			// Key structure sizes from this package
			"CompilationResult_B", unsafe.Sizeof(CompilationResult{}),
			// Proxy estimates for vendor structs
			"progProxy_B", unsafe.Sizeof(progProxy{}),
			// Dispatcher internals (from interpreter/dispatcher.go):
			//   defaultDispatcher{parent Dispatcher(2×ptr), overloads map(8B header)} = ~24 B header
			//   map[string]*functions.Overload: header 8 B, each bucket ~128 B
			"dispatcherHeader_B", 24,
			// functions.Overload (from common/functions/functions.go):
			//   Operator string(16) + OperandTrait int(8) + Unary func(8) +
			//   Binary func(8) + Function func(8) + NonStrict bool(1) + pad = ~56 B
			"functionsOverload_B", 56,
			// Per-overload total cost in dispatcher (map entry + Overload struct):
			//   ~72 B pointer + 56 B struct + ~72 B map bucket overhead ≈ 200 B
			"dispatcherEntryTotal_B", 200,
			// With 345 overloads (unpatched): 345 × 200 = 69 KB per program
			// With ~10 overloads (patched):   10 × 200 = 2 KB per program
			"dispatchCost_unpatched_KB", 345*200/1024,
			"dispatchCost_patched_KB", 10*200/1024,
			// At 5000 policies × 5 expressions = 25000 programs:
			"dispatchCostTotal_unpatched_MB", 25000*345*200/1024/1024,
			"dispatchCostTotal_patched_MB", 25000*10*200/1024/1024,
		)
	})
}

func init() {
	logCELStructSizes()
}
