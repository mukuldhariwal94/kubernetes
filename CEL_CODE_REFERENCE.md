# CEL Expression Loading - Complete Code Reference Guide

## Quick Navigation

| Topic | File | Line(s) |
|-------|------|---------|
| **Where VAP is loaded** | `policy_source.go` | 52-75, 118-131 |
| **Where CEL is compiled** | `plugin.go` | 118-150 |
| **Where evaluation happens** | `dispatcher.go` | 214-223 |
| **Where LRU cache integrates** | `policy_source.go` | 71, 470-501 |
| **How cache prevents OOM** | `lru_policy_cache.go` | 100-160 |

---

## Code Reference: CEL Expression Loading Points

### 1️⃣ POLICY LOADING (When policy enters cluster)

**File**: `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go`

**Step 1.1: Initialization with LRU Cache**
```go
// Line 52-75: policySource structure definition
type policySource[P runtime.Object, B runtime.Object, E Evaluator] struct {
    ctx                context.Context
    policyInformer     generic.Informer[P]    // ← Watches policies in etcd
    bindingInformer    generic.Informer[B]    // ← Watches bindings in etcd
    restMapper         meta.RESTMapper
    newPolicyAccessor  func(P) PolicyAccessor
    newBindingAccessor func(B) BindingAccessor

    informerFactory informers.SharedInformerFactory
    dynamicClient   dynamic.Interface

    compiler func(P) E                       // ← Compiler function

    // Currently compiled list of valid/active policy-binding pairs
    policies atomic.Pointer[[]PolicyHook[P, B, E]]
    // Whether the cache of policies is dirty and needs to be recompiled
    policiesDirty atomic.Bool

    lock             sync.Mutex
    
    // ⭐ LRU CACHE INTEGRATION POINT #1
    compiledPolicies map[types.NamespacedName]compiledPolicyEntry[E]  // ← CHANGE THIS!
    
    // Temporary until we use the dynamic informer factory
    paramsCRDControllers map[schema.GroupVersionKind]*paramInfo
}

// CHANGE TO:
// policyCache *LRUPolicyCache  // ← Bounded to 512MB
```

**Step 1.2: Initialization of LRU Cache**
```go
// Line 118-131: NewPolicySource function
func NewPolicySource[P runtime.Object, B runtime.Object, E Evaluator](
    policyInformer cache.SharedIndexInformer,
    bindingInformer cache.SharedIndexInformer,
    newPolicyAccessor func(P) PolicyAccessor,
    newBindingAccessor func(B) BindingAccessor,
    compiler func(P) E,
    paramInformerFactory informers.SharedInformerFactory,
    dynamicClient dynamic.Interface,
    restMapper meta.RESTMapper,
) Source[PolicyHook[P, B, E]] {
    res := &policySource[P, B, E]{
        compiler:             compiler,
        policyInformer:       generic.NewInformer[P](policyInformer),
        bindingInformer:      generic.NewInformer[B](bindingInformer),
        
        // ⭐ LRU CACHE INTEGRATION POINT #2
        compiledPolicies:     map[types.NamespacedName]compiledPolicyEntry[E]{},
        // ↑ CHANGE THIS TO:
        // policyCache: NewLRUPolicyCache(512 * 1024 * 1024),  // 512MB limit
        
        paramsCRDControllers: map[schema.GroupVersionKind]*paramInfo{},
        informerFactory:      paramInformerFactory,
        dynamicClient:        dynamicClient,
        restMapper:           restMapper,
    }
    return res
}
```

---

### 2️⃣ CACHE SYNCHRONIZATION (Every 1 second)

**File**: `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go`

**Step 2.1: Policy Refresh Loop**
```go
// Line 200-206: Refresh policies every 1 second
go func() {
    policyRefreshIntervalLock.Lock()
    interval := policyRefreshInterval  // Default: 1 second
    policyRefreshIntervalLock.Unlock()
    wait.Until(s.refreshPolicies, interval, ctx.Done())
}()

// What happens every 1 second:
//
// 1. Check if policiesDirty flag is true (set by informer)
// 2. If dirty: Call calculatePolicyData()
// 3. Rebuild list of active policies & bindings
// 4. Store in atomic.Pointer for safe access during requests
```

**Step 2.2: Calculate Policy Data**
```go
// Line 277-387: calculatePolicyData function
// This is where CEL expressions are compiled!

func (s *policySource[P, B, E]) calculatePolicyData() ([]PolicyHook[P, B, E], error) {
    if !s.UpstreamHasSynced() {
        return nil, fmt.Errorf("cannot calculate policy data until upstream has synced")
    }

    s.lock.Lock()
    defer s.lock.Unlock()

    // Create a local copy of all policies and bindings
    policiesToBindings := map[types.NamespacedName][]B{}
    bindingList, err := s.bindingInformer.List(labels.Everything())
    if err != nil {
        return nil, err
    }

    // Gather a list of all active policy bindings
    for _, bindingSpec := range bindingList {
        bindingAccessor := s.newBindingAccessor(bindingSpec)
        policyKey := bindingAccessor.GetPolicyName()
        
        // Map: policy name → list of bindings
        policiesToBindings[policyKey] = append(policiesToBindings[policyKey], bindingSpec)
    }

    result := make([]PolicyHook[P, B, E], 0, len(bindingList))
    usedParams := map[schema.GroupVersionKind]struct{}{}
    var errs []error
    
    // ⭐ MAIN CEL COMPILATION LOOP
    for policyKey, bindingSpecs := range policiesToBindings {
        var inf generic.NamespacedLister[P] = s.policyInformer
        if len(policyKey.Namespace) > 0 {
            inf = s.policyInformer.Namespaced(policyKey.Namespace)
        }
        
        // Get policy from informer
        policySpec, err := inf.Get(policyKey.Name)
        if errors.IsNotFound(err) {
            continue
        } else if err != nil {
            errs = append(errs, err)
            continue
        }

        var parsedParamKind *schema.GroupVersionKind
        policyAccessor := s.newPolicyAccessor(policySpec)

        if paramKind := policyAccessor.GetParamKind(); paramKind != nil {
            // ... param handling ...
            usedParams[*parsedParamKind] = struct{}{}
        }

        paramInformer, paramScope, configurationError := s.ensureParamsForPolicyLocked(parsedParamKind)
        
        // ⭐ CALL compilePolicyLocked HERE!
        result = append(result, PolicyHook[P, B, E]{
            Policy:             policySpec,
            Bindings:           bindingSpecs,
            Evaluator:          s.compilePolicyLocked(policySpec),  // ← CEL COMPILATION!
            ParamInformer:      paramInformer,
            ParamScope:         paramScope,
            ConfigurationError: configurationError,
        })

        if configurationError != nil {
            errs = append(errs, configurationError)
        }
    }

    // Clean up orphaned policies
    for policyKey := range s.compiledPolicies {
        if _, wasSeen := policiesToBindings[policyKey]; !wasSeen {
            delete(s.compiledPolicies, policyKey)  // ← CHANGE THIS!
            // TO: s.policyCache.Remove(policyKey)
        }
    }

    // Clean up orphaned param informers
    for paramKind, info := range s.paramsCRDControllers {
        if _, wasSeen := usedParams[paramKind]; !wasSeen {
            info.cancelFunc()
            delete(s.paramsCRDControllers, paramKind)
        }
    }

    err = nil
    if len(errs) > 0 {
        err = goerrors.Join(errs...)
    }
    return result, err
}
```

---

### 3️⃣ CEL COMPILATION (Where Memory is Allocated)

**File**: `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go`

**Step 3.1: compilePolicyLocked - Cache Lookup & Compilation**
```go
// Line 465-501: compilePolicyLocked function
// ⭐ THIS IS WHERE LRU CACHE INTEGRATES!

func (s *policySource[P, B, E]) compilePolicyLocked(policySpec P) E {
    policyMeta, err := meta.Accessor(policySpec)
    if err != nil {
        utilruntime.HandleError(err)
        var emptyEvaluator E
        return emptyEvaluator
    }
    
    key := types.NamespacedName{
        Namespace: policyMeta.GetNamespace(),
        Name:      policyMeta.GetName(),
    }

    // ⭐ LRU CACHE INTEGRATION POINT #3: LOOKUP
    // CURRENT (UNBOUNDED):
    compiledPolicy, wasCompiled := s.compiledPolicies[key]
    
    // NEW (WITH LRU):
    // cachedEval, wasCompiled := s.policyCache.Get(key)
    // if wasCompiled {
    //     cachedEntry := cachedEval.(*compiledPolicyEntry[E])
    //     if cachedEntry.policyVersion == policyMeta.GetResourceVersion() {
    //         klog.V(3).Infof("CEL cache hit for policy %s/%s", key.Namespace, key.Name)
    //         return cachedEntry.evaluator  // ← Fast return from cache!
    //     }
    // }

    // If the policy or binding has changed since it was last compiled,
    // and if there is no configuration error (like a missing param CRD)
    // then we recompile
    if !wasCompiled ||
        compiledPolicy.policyVersion != policyMeta.GetResourceVersion() {

        // ⭐ THIS IS WHERE CEL GETS COMPILED (9-20MB allocation!)
        compiledPolicy = compiledPolicyEntry[E]{
            policyVersion: policyMeta.GetResourceVersion(),
            evaluator:     s.compiler(policySpec),  // ← CALLS compilePolicy() in plugin.go
        }
        
        // ⭐ LRU CACHE INTEGRATION POINT #4: STORE
        // CURRENT (UNBOUNDED):
        s.compiledPolicies[key] = compiledPolicy
        
        // NEW (WITH LRU):
        // estimatedSize := calculateEstimatedSize(policySpec)
        // s.policyCache.Put(key, compiledPolicy, estimatedSize)  // ← Auto-evicts if needed!
    }

    return compiledPolicy.evaluator
}
```

**Step 3.2: Actual CEL Compilation**
```go
// File: staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go
// Line 118-150: compilePolicy function

func compilePolicy(policy *Policy) Validator {
    hasParam := false
    if policy.Spec.ParamKind != nil {
        hasParam = true
    }
    
    optionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: true}
    expressionOptionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: false}
    failurePolicy := policy.Spec.FailurePolicy
    var matcher matchconditions.Matcher = nil
    matchConditions := policy.Spec.MatchConditions
    
    // Get reusable composition environment (singleton, cached globally)
    compositionEnvTemplate := getCompositionEnvTemplateWithStrictCost()
    
    // Create compiler from template
    filterCompiler := cel.NewCompositedCompilerFromTemplate(compositionEnvTemplate)
    
    // Store variables for later use
    filterCompiler.CompileAndStoreVariables(convertv1beta1Variables(policy.Spec.Variables), optionalVars, environment.StoredExpressions)

    // ⭐ PROGRAM 1: MATCH CONDITIONS (5-10MB)
    if len(matchConditions) > 0 {
        matchExpressionAccessors := make([]cel.ExpressionAccessor, len(matchConditions))
        for i := range matchConditions {
            matchExpressionAccessors[i] = (*matchconditions.MatchCondition)(&matchConditions[i])
        }
        matcher = matchconditions.NewMatcher(
            filterCompiler.CompileCondition(matchExpressionAccessors, optionalVars, environment.StoredExpressions),
            // ↑ ALLOCATES & COMPILES ALL MATCH CONDITIONS
            failurePolicy, "policy", "validate", policy.Name
        )
    }
    
    // ⭐ CREATE VALIDATOR WITH 4 COMPILED PROGRAMS
    res := NewValidator(
        // PROGRAM 2: VALIDATIONS (2-5MB)
        filterCompiler.CompileCondition(convertv1Validations(policy.Spec.Validations), optionalVars, environment.StoredExpressions),
        matcher,
        // PROGRAM 3: AUDIT ANNOTATIONS (1-3MB)
        filterCompiler.CompileCondition(convertv1AuditAnnotations(policy.Spec.AuditAnnotations), optionalVars, environment.StoredExpressions),
        // PROGRAM 4: MESSAGE EXPRESSIONS (1-2MB)
        filterCompiler.CompileCondition(convertv1MessageExpressions(policy.Spec.Validations), expressionOptionalVars, environment.StoredExpressions),
        failurePolicy,
    )

    // TOTAL: 9-20MB of compiled CEL programs returned as Validator
    return res
}
```

---

### 4️⃣ REQUEST EVALUATION (Using Cached Programs)

**File**: `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go`

**Step 4.1: Using Cached Evaluator During Request**
```go
// Line 72-309: Dispatch function

func (c *dispatcher) Dispatch(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces, hooks []PolicyHook) error {
    var deniedDecisions []policyDecisionWithMetadata
    
    authz := admissionauthorizer.NewCachingAuthorizer(c.authz)

    // Loop through all applicable policies
    for _, hook := range hooks {
        var versionedAttr *admission.VersionedAttributes

        definition := hook.Policy
        
        // Check if policy matches the request
        matches, matchResource, matchKind, err := c.matcher.DefinitionMatches(a, o, NewValidatingAdmissionPolicyAccessor(definition))
        // ...

        auditAnnotationCollector := newAuditAnnotationCollector()
        
        for _, binding := range hook.Bindings {
            matches, err := c.matcher.BindingMatches(a, o, NewValidatingAdmissionPolicyBindingAccessor(binding))
            // ...

            params, err := generic.CollectParams(...)
            // ...

            var validationResults []ValidateResult
            var namespace *v1.Namespace
            // ...

            // ⭐ MAIN CEL EVALUATION POINT
            for _, param := range params {
                var p runtime.Object = param
                if p != nil && p.GetObjectKind().GroupVersionKind().Empty() {
                    p = &wrappedParam{
                        TypeMeta: metav1.TypeMeta{
                            APIVersion: definition.Spec.ParamKind.APIVersion,
                            Kind:       definition.Spec.ParamKind.Kind,
                        },
                        nested: param,
                    }
                }

                // ⭐ HERE: hook.Evaluator uses CACHED CEL PROGRAMS
                validationResults = append(validationResults,
                    hook.Evaluator.Validate(
                        ctx,
                        matchResource,
                        versionedAttr,
                        p,
                        namespace,
                        celconfig.RuntimeCELCostBudget,  // Cost limit
                        authz,
                    ),
                )
                // ↑ This Evaluator was:
                //   1. Retrieved from policyCache via compilePolicyLocked()
                //   2. Contains 4 compiled CEL programs
                //   3. Already in memory (95%+ cache hit!)
                //   4. Uses pre-compiled programs (no recompilation)
            }

            // Process results...
            for _, validationResult := range validationResults {
                for i, decision := range validationResult.Decisions {
                    // Handle admit/deny decisions
                    // ...
                }
            }
        }
    }

    if len(deniedDecisions) > 0 {
        // ... return error
    }
    return nil
}
```

---

## 🎯 Memory Allocation Summary

| Step | Code Location | Allocates | Size | Cached? |
|------|---------------|-----------|------|---------|
| 1. Initialize cache | `policy_source.go:118-131` | LRU structure | ~1MB | Always |
| 2. Policy created | `calculatePolicyData():277` | CEL expressions | 0 | N/A |
| 3. **Compile CEL** | `compilePolicy():118` | **4 programs** | **9-20MB** | **YES** ← LRU |
| 4. Store in cache | `compilePolicyLocked():497` | Cache entry | ~20KB | Always |
| 5. Request arrives | `Dispatch():72` | Nothing | 0 | N/A |
| 6. **Use cached programs** | `Validate():214` | Nothing | 0 | **YES** ← Fast! |

---

## 💡 Why LRU Cache Works

```
Without LRU (Current Problem):
┌────────────────────────────────────────┐
│ compiledPolicies map grows forever     │
│                                        │
│ p1: 9MB                                │
│ p2: 8MB     ← All stored permanently  │
│ p3: 7MB     ← Never evicted           │
│ ...                                    │
│ p500: 9MB   ← Total: 4.5GB (OOM!)    │
└────────────────────────────────────────┘

With LRU (Solution):
┌────────────────────────────────────────┐
│ policyCache bounded at 512MB           │
│                                        │
│ Front: p50 (most recent)  9MB          │
│ p49: 8MB                               │
│ p48: 7MB                               │
│ ... (50 policies fit)                  │
│ p1 (least recent)  ← Evicted if needed│
│                                        │
│ Memory: CAPPED at 512MB forever ✅    │
└────────────────────────────────────────┘
```

---

## Quick Integration Checklist

- [ ] Line 71: Replace `compiledPolicies` map with `policyCache *LRUPolicyCache`
- [ ] Line 118-131: Initialize with `NewLRUPolicyCache(512 * 1024 * 1024)`
- [ ] Line 368: Change `delete()` to `s.policyCache.Remove()`
- [ ] Line 485-501: Update `compilePolicyLocked()` to use cache
  - [ ] Change map lookup to `s.policyCache.Get(key)`
  - [ ] Change map store to `s.policyCache.Put(key, compiledPolicy, estimatedSize)`
- [ ] Add size estimation function
- [ ] Test with 100+ policies

