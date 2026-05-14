# ValidatingAdmissionPolicy — End-to-End Deep Dive

A code-level walkthrough of how CEL-based `ValidatingAdmissionPolicy` (VAP) is
processed inside kube-apiserver, from an inbound REST request through etcd
storage, informer sync, CEL compilation, binding matching, evaluation, and
decision.

All file paths are relative to the repo root. References use the form
`file:line`.

---

## 1. End-to-end request lifecycle

### 1.1 The request path, top down

```
kubectl apply -f deploy.yaml
       │
       ▼
POST /apis/apps/v1/namespaces/prod/deployments
       │  TLS + client-cert / token auth
       ▼
┌──────────────────────────────────────────────────────────────────────┐
│  kube-apiserver handler chain                                        │
│  (staging/src/k8s.io/apiserver/pkg/server/filters)                   │
│                                                                      │
│   WithPanicRecovery                                                  │
│   WithRequestInfo        — resolves Resource/Verb/Namespace          │
│   WithAuthentication     — sets user.Info on ctx                     │
│   WithAuthorization      — RBAC / Node / ABAC                        │
│   WithImpersonation      — optional                                  │
│   WithMaxInFlightLimit   — priority-and-fairness                     │
│   WithAudit              — audit events                              │
│   WithWatchTerminationDuringShutdown                                 │
│                                                                      │
│                 │ (passes to REST handlers)                          │
│                 ▼                                                    │
│   endpoints/handlers/create.go · createHandler                       │
│     1. decode body -> runtime.Object                                 │
│     2. admission.MutatingAdmission   ← CHAIN 1                       │
│     3. rest.BeforeCreate             ← defaulting / validation       │
│     4. admission.ValidatingAdmission ← CHAIN 2  ◄── VAP runs here    │
│     5. Storage.Create               ← etcd writes                    │
└──────────────────────────────────────────────────────────────────────┘
```

The two admission **chains** are built once at server startup and wrapped
around every REST write. VAP is a `ValidationInterface` plugin, so it runs
in the **validating** chain only, after all mutation plugins have finished
and after scheme-level defaulting/validation.

### 1.2 Mutation vs. validation — precise distinction

Defined in [staging/src/k8s.io/apiserver/pkg/admission/interfaces.go:129-144](staging/src/k8s.io/apiserver/pkg/admission/interfaces.go#L129-L144):

```go
type MutationInterface interface {
    Interface
    Admit(ctx context.Context, a Attributes, o ObjectInterfaces) (err error)
}

type ValidationInterface interface {
    Interface
    Validate(ctx context.Context, a Attributes, o ObjectInterfaces) (err error)
}
```

[chain.go:31-44](staging/src/k8s.io/apiserver/pkg/admission/chain.go#L31-L44) walks the chain:

```go
func (admissionHandler chainAdmissionHandler) Admit(ctx, a, o) error {
    for _, handler := range admissionHandler {
        if !handler.Handles(a.GetOperation()) { continue }
        if mutator, ok := handler.(MutationInterface); ok {
            if err := mutator.Admit(ctx, a, o); err != nil { return err }
        }
    }
    return nil
}
```

VAP's top-level plugin `*validating.Plugin` embeds `*generic.Plugin[PolicyHook]`,
which only implements `ValidationInterface.Validate`. Its `Validate` is the
adapter into the VAP machinery. See [validating/plugin.go:143](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L143):

```go
func (a *Plugin) Validate(ctx context.Context, attr admission.Attributes, o admission.ObjectInterfaces) error {
    return a.Plugin.Dispatch(ctx, attr, o)   // delegates to generic.Plugin.Dispatch
}
```

### 1.3 Logical architecture

```
 ┌────────────────────┐    ┌────────────────────────┐
 │ REST write request │───▶│ createHandler          │
 └────────────────────┘    │  (endpoints/handlers)  │
                           └───────────┬────────────┘
                                       │
                 ┌─────────────────────┼─────────────────────┐
                 ▼                     ▼                     ▼
       MutatingAdmission       BeforeCreate         ValidatingAdmission
       (chainAdmissionHandler) (scheme defaults &   (chainAdmissionHandler)
                                validation)
                                                             │
                                 ┌───────────────────────────┘
                                 ▼
                     ┌──────────────────────────────────────┐
                     │ for each handler in chain:           │
                     │   if handler.Handles(op):            │
                     │     if ValidationInterface: Validate │
                     └──────────────────────────────────────┘
                                 │
                                 ├──────────▶ ResourceQuota
                                 ├──────────▶ CertificateSubjectRestriction
                                 ├──────────▶ ValidatingAdmissionWebhook
                                 └──────────▶ **ValidatingAdmissionPolicy** ◄── our path
                                                      │
                                                      ▼
                                            generic.Plugin.Dispatch
                                                      │
                                                      ▼
                                            policyDispatcher / validating.dispatcher
                                                      │
                                                      ▼
                                            validator.Validate (per policy+binding)
                                                      │
                                                      ▼
                                            CEL evaluation (per expression)
```

---

## 2. Generic admission framework — entry point

### 2.1 Attributes: the request, captured

[admission/interfaces.go:31-77](staging/src/k8s.io/apiserver/pkg/admission/interfaces.go#L31-L77):

```go
type Attributes interface {
    GetName() string
    GetNamespace() string
    GetResource() schema.GroupVersionResource
    GetSubresource() string
    GetOperation() Operation            // CREATE / UPDATE / DELETE / CONNECT
    GetOperationOptions() runtime.Object
    IsDryRun() bool
    GetObject() runtime.Object          // incoming object (nil for DELETE)
    GetOldObject() runtime.Object       // existing object (nil for CREATE)
    GetKind() schema.GroupVersionKind
    GetUserInfo() user.Info
    AddAnnotation(key, value string) error
    AddAnnotationWithLevel(key, value string, level auditinternal.Level) error
    GetReinvocationContext() ReinvocationContext
}
```

This is the universal packet passed to every admission plugin. The concrete
implementation is [`admission.attributesRecord`](staging/src/k8s.io/apiserver/pkg/admission/attributes.go), constructed by
[`admission.NewAttributesRecord`](staging/src/k8s.io/apiserver/pkg/admission/attributes.go#L28) inside the REST handler.

### 2.2 VersionedAttributes — the CEL-ready wrapper

For CEL we need the object in a specific GVK (the one the policy asks about,
possibly via `matchPolicy: Equivalent` resource equivalence). The generic
dispatcher wraps `Attributes` into `*admission.VersionedAttributes` via
[`admission.NewVersionedAttributes`](staging/src/k8s.io/apiserver/pkg/admission/conversion.go). `VersionedAttributes.VersionedObject`
and `VersionedOldObject` are what CEL activations read.

Conversion is lazy and cached per matched-GVK in
[generic/policy_dispatcher.go:386-402](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_dispatcher.go#L386-L402):

```go
func (v *versionedAttributeAccessor) VersionedAttribute(gvk schema.GroupVersionKind) (*admission.VersionedAttributes, error) {
    if val, ok := v.versionedAttrs[gvk]; ok { return val, nil }
    versionedAttr, err := admission.NewVersionedAttributes(v.attr, gvk, v.objectInterfaces)
    if err != nil { return nil, err }
    v.versionedAttrs[gvk] = versionedAttr
    return versionedAttr, nil
}
```

So a single admission request performs object conversion **once per
distinct GVK** requested by matching policies, even across many bindings.

### 2.3 How admission plugins register

The stock set is wired via [`DefaultOffAdmissionPlugins`](staging/src/k8s.io/apiserver/pkg/server/options/admission.go)
and explicit registration calls such as [`validating.Register(plugins)`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L60-L64):

```go
func Register(plugins *admission.Plugins) {
    plugins.Register(PluginName, func(configFile io.Reader) (admission.Interface, error) {
        return NewPlugin(configFile)
    })
}
```

The server-side options pass a `pluginNames` list; `plugins.NewFromPlugins`
materializes the chain, injects initializers (informer factory, client,
authorizer, RESTMapper, dynamic client), and hands the resulting
`chainAdmissionHandler` to the generic control plane.

---

## 3. ValidatingAdmissionPolicy plugin flow

Two "plugin" layers exist. The **outer** one is generic and reused by VAP and
MAP; the **inner** one is family-specific.

### 3.1 Outer — generic.Plugin[H]

[`generic/plugin.go:69-107`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/plugin.go#L69-L107):

```go
type Plugin[H any] struct {
    *admission.Handler

    apiSourceFactory    apiSourceFactory[H]     // builds informer-backed source
    dispatcherFactory   dispatcherFactory[H]    // builds per-family dispatcher
    staticSourceFactory StaticSourceFactory[H]  // optional: static manifests

    source     Source[H]
    dispatcher Dispatcher[H]
    matcher    *matching.Matcher

    staticSource HookSource[H]
    staticManifestsDir string
    apiServerID string

    informerFactory informers.SharedInformerFactory
    client          kubernetes.Interface
    restMapper      meta.RESTMapper
    dynamicClient   dynamic.Interface

    admissionConfigResources sets.Set[schema.GroupResource]
    excludedResources        sets.Set[schema.GroupResource]
    stopCh      <-chan struct{}
    authorizer  authorizer.Authorizer
    enabled     bool
}
```

`H` is the per-family hook type. For VAP it's
`PolicyHook = generic.PolicyHook[*v1.ValidatingAdmissionPolicy, *v1.ValidatingAdmissionPolicyBinding, Validator]`.

Initialization happens in
[`ValidateInitialization`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/plugin.go#L203):

1. Validates all injected deps are present (informer factory, client,
   authorizer, etc.) — if any are missing, returns an error that short-circuits
   apiserver startup.
2. Creates a `*matching.Matcher` using the core/v1 Namespace lister — used
   both by the outer matcher (for `namespaceSelector`) and by the inner
   dispatcher.
3. Builds the source from `apiSourceFactory`. For VAP this is constructed
   in [`validating/plugin.go:120-130`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L120-L130):
   ```go
   generic.NewPolicySource(
       f.Admissionregistration().V1().ValidatingAdmissionPolicies().Informer(),
       f.Admissionregistration().V1().ValidatingAdmissionPolicyBindings().Informer(),
       NewValidatingAdmissionPolicyAccessor,
       NewValidatingAdmissionPolicyBindingAccessor,
       compilePolicy,              // ← the CEL compile entry point
       f, dynamicClient, restMapper,
   )
   ```
4. Starts `source.Run(pluginContext)` in a goroutine. This is the continuous
   informer-driven compile loop (covered in §4).
5. Installs a `SetReadyFunc` that gates admission until the Namespace and
   policy informers have synced — if readiness is false, admission returns
   `NewForbidden(... "not yet ready to handle request")` via
   [`generic/plugin.go:330-332`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/plugin.go#L330-L332).
6. Constructs the family-specific dispatcher via `dispatcherFactory` —
   for VAP that's `validating.NewDispatcher(authorizer, matcher)`.

### 3.2 Outer Dispatch — per-request entry

[`generic/plugin.go:302-335`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/plugin.go#L302-L335):

```go
func (c *Plugin[H]) Dispatch(ctx, a, o) (err error) {
    if !c.enabled { return nil }
    gr := a.GetResource().GroupResource()

    if c.isExcludedFromAllHooks(gr) { return nil }    // auth/authz reviews
    if c.isExcludedFromAPIHooks(gr) {                  // VAP/MAP/VAPB/MAPB
        if c.staticSource != nil { ... }               // static policies only
        return nil
    }

    if !c.WaitForReady() {
        return admission.NewForbidden(a, fmt.Errorf("not yet ready to handle request"))
    }
    return c.dispatcher.Dispatch(ctx, a, o, c.source.Hooks())
}
```

Two exclusions matter:

- `excludedResources` — e.g. `*reviews.authentication.k8s.io`,
  `*reviews.authorization.k8s.io`. Never subject to any policy hook (would
  cause recursion).
- `admissionConfigResources` — VAP, MAP, VAPB, MAPB themselves. Skipped by
  API-backed policies to prevent circular dependency (a broken VAP protecting
  VAPs would lock out fixes). Static manifest-based policies are still
  evaluated because they can't deadlock themselves.

### 3.3 Inner policyDispatcher — generic fan-out

[`generic/policy_dispatcher.go:110-205`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_dispatcher.go#L110-L205) is the engine that converts
"all hooks" into "invocations relevant to this request":

```go
func (d *policyDispatcher[P, B, E]) Dispatch(ctx, a, o, hooks []PolicyHook[P, B, E]) error {
    var relevantHooks []PolicyInvocation[P, B, E]
    versionedAttrAccessor := &versionedAttributeAccessor{ ... }

    for _, hook := range hooks {
        policyAccessor := d.newPolicyAccessor(hook.Policy)
        matches, matchGVR, matchGVK, err := d.matcher.DefinitionMatches(a, o, policyAccessor)
        if !matches { continue }                 // top-level matchConstraints miss
        if hook.ConfigurationError != nil { addConfigError(...); continue }

        for _, binding := range hook.Bindings {
            bindingAccessor := d.newBindingAccessor(binding)
            matches, err = d.matcher.BindingMatches(a, o, bindingAccessor)
            if !matches { continue }

            versionedAttrAccessor.VersionedAttribute(matchGVK)   // cache warm

            params, err := CollectParams(                         // fetch paramRef
                policyAccessor.GetParamKind(), hook.ParamInformer,
                hook.ParamScope, bindingAccessor.GetParamRef(),
                a.GetNamespace())
            ...
            for _, param := range params {
                relevantHooks = append(relevantHooks, PolicyInvocation[...]{
                    Policy: hook.Policy, Binding: binding,
                    Kind: matchGVK, Resource: matchGVR,
                    Param: param, Evaluator: hook.Evaluator,
                })
            }
        }
    }

    if len(relevantHooks) > 0 {
        extraPolicyErrors, statusError := d.delegate(ctx, a, o, versionedAttrAccessor, relevantHooks)
        ...
    }
    // FailurePolicy filtering on config errors
    ...
}
```

**Important properties:**

- Policies are evaluated against **every matching binding** separately.
  A single policy with 3 bindings produces up to 3 invocations.
- A binding with a `paramRef` selector that matches K param objects produces
  K invocations — one per parameter object.
- Config errors (param CRD missing, paramRef malformed) are bucketed into
  `policyErrors`. They go through `FailurePolicy` filtering: `Fail` keeps
  them and converts to 403; `Ignore` drops them silently.

Note: for VAP, the `delegate` plumbed into this generic path is **not** used.
`validating.NewDispatcher` returns its own `dispatcher` which does the same
outer loop but with validation-specific decision accounting (audit
annotations, `validationActions: Deny/Warn/Audit`, etc.). See [validating/dispatcher.go:72](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L72).

### 3.4 Inner validating.dispatcher — per-family accounting

Key responsibilities, in order:

1. **Top-level match** — [validating/dispatcher.go:127](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L127)
   `c.matcher.DefinitionMatches(...)` — evaluates `spec.matchConstraints`
   (namespaceSelector, objectSelector, resourceRules, excludeResourceRules,
   matchPolicy).
2. **Binding-level match** — [validating/dispatcher.go:146](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L146)
   `c.matcher.BindingMatches(...)` — evaluates
   `binding.spec.matchResources` (same shape, scoped to binding).
3. **Param collection** — [validating/dispatcher.go:156](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L156) delegates to
   `generic.CollectParams`, which reads from the per-param-kind informer
   built by `policySource.ensureParamsForPolicyLocked`.
4. **Versioned conversion** — [validating/dispatcher.go:170](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L170)
   `admission.NewVersionedAttributes(a, matchKind, o)`. Converts the
   request object to the matched GVK if needed.
5. **Namespace fetch** — [validating/dispatcher.go:192](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L192)
   `c.matcher.GetNamespace(ctx, namespaceName)`. Goes through the
   namespace lister (cached) so this is an in-memory hit on the hot path.
6. **Per-param evaluate** — [validating/dispatcher.go:213](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L213):
   ```go
   hook.Evaluator.Validate(ctx, matchResource, versionedAttr, p, namespace,
                           celconfig.RuntimeCELCostBudget, authz)
   ```
7. **Action dispatch** — [validating/dispatcher.go:226-283](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L226-L283)
   iterates `validationResult.Decisions`. For each denial, consults
   `binding.Spec.ValidationActions`:
   - `Deny` — appended to `deniedDecisions`; caller returns 403.
   - `Warn`  — `warning.AddWarning(ctx, ...)` surfaces in `kubectl`.
   - `Audit` — stamps `validation.policy.admission.k8s.io/validation_failure`
     as a JSON-marshalled audit annotation.
8. **Audit annotations** — [validating/dispatcher.go:257-283](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L257-L283)
   collects `auditAnnotationFilter` outputs into an `auditAnnotationCollector`,
   de-duplicating identical values.
9. **Finalize** — [validating/dispatcher.go:289-307](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L289-L307) constructs a
   `StatusError` with `Reason`, numeric code, and `Causes[]`, each cause
   being one denied policy.

---

## 4. etcd → policy loading path

### 4.1 Storage

VAP and VAPB are standard Kubernetes resources. Their REST storage lives at
[pkg/registry/admissionregistration/validatingadmissionpolicy/storage/](pkg/registry/admissionregistration/validatingadmissionpolicy/storage/)
and uses the shared `genericregistry.Store`, which serializes through
`runtime.Serializer` and writes to etcd via `storage.Interface`
(typically `etcd3`). Validation is driven by
[`pkg/registry/admissionregistration/validatingadmissionpolicy/strategy.go`](pkg/registry/admissionregistration/validatingadmissionpolicy/strategy.go)
(structural CEL validation, costs, etc.). Once stored, VAP objects look like
any other Kubernetes custom resource.

### 4.2 Informers

When kube-apiserver boots, `f.Admissionregistration().V1().ValidatingAdmissionPolicies()`
returns a typed `SharedInformer` created by the shared informer factory. Each
informer:

- Starts a `ListAndWatch` against the REST store at boot
  (initial list drained into an in-memory `cache.Store`).
- Keeps a long-lived `watch` connection receiving `ADDED/MODIFIED/DELETED`
  events and updating the cache.
- Provides `Lister()` and `HasSynced()`.

The VAP plugin consumes both the policy and binding informers in
[`validating/plugin.go:122-123`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L122-L123).

### 4.3 policySource — the refresh engine

[generic/policy_source.go:52-75](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L52-L75):

```go
type policySource[P runtime.Object, B runtime.Object, E Evaluator] struct {
    ctx                context.Context
    policyInformer     generic.Informer[P]
    bindingInformer    generic.Informer[B]
    restMapper         meta.RESTMapper
    newPolicyAccessor  func(P) PolicyAccessor
    newBindingAccessor func(B) BindingAccessor

    informerFactory informers.SharedInformerFactory
    dynamicClient   dynamic.Interface

    compiler func(P) E

    policies      atomic.Pointer[[]PolicyHook[P, B, E]]
    policiesDirty atomic.Bool

    lock             sync.Mutex
    compiledPolicies map[types.NamespacedName]compiledPolicyEntry[E]

    paramsCRDControllers map[schema.GroupVersionKind]*paramInfo
}
```

The `Run` loop — [generic/policy_source.go:146-210](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L146-L210):

```
Run(ctx):
  WaitForNamedCacheSyncWithContext(ctx, UpstreamHasSynced)
  refreshPolicies()                           // initial pass
  AddEventHandler(notify) on policy informer
  AddEventHandler(notify) on binding informer
  go wait.Until(refreshPolicies, 1*time.Second, ctx.Done())
  <-ctx.Done()
```

Any informer event calls `notify()`, which flips `policiesDirty = true`. The
1-Hz worker reads-and-clears the flag and runs `refreshPolicies()`:

[generic/policy_source.go:238-264](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L238-L264):

```go
func (s *policySource[P, B, E]) refreshPolicies() {
    if !s.UpstreamHasSynced() { return }
    if !s.policiesDirty.Swap(false) { return }

    policies, err := s.calculatePolicyData()
    s.policies.Store(&policies)         // atomic publish
    if err != nil {
        utilruntime.HandleError(...)
        s.notify()                       // retry
    }
}
```

### 4.4 calculatePolicyData — building the Hooks slice

[generic/policy_source.go:277-387](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L277-L387), simplified:

```go
// 1. List all bindings, group by referenced policy name.
policiesToBindings[bindingAccessor.GetPolicyName()] = append(..., binding)

// 2. For each policy referenced by at least one binding:
for policyKey, bindingSpecs := range policiesToBindings {
    policySpec := policyInformer.Get(policyKey.Name)       // from cache
    parsedParamKind := parse(policySpec.Spec.ParamKind)
    paramInformer, paramScope, cfgErr := ensureParamsForPolicyLocked(parsedParamKind)

    result = append(result, PolicyHook{
        Policy:             policySpec,
        Bindings:           bindingSpecs,
        Evaluator:          compilePolicyLocked(policySpec),  // cached by RV
        ParamInformer:      paramInformer,
        ParamScope:         paramScope,
        ConfigurationError: cfgErr,
    })
}

// 3. Evict stale entries from compiledPolicies and stop unused param informers.
```

### 4.5 compile cache keyed by resourceVersion

[generic/policy_source.go:470-501](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L470-L501):

```go
compiledPolicy, wasCompiled := s.compiledPolicies[key]
if !wasCompiled || compiledPolicy.policyVersion != policyMeta.GetResourceVersion() {
    compiledPolicy = compiledPolicyEntry[E]{
        policyVersion: policyMeta.GetResourceVersion(),
        evaluator:     s.compiler(policySpec),
    }
    s.compiledPolicies[key] = compiledPolicy
}
return compiledPolicy.evaluator
```

**Implication:** compile happens only when the policy's `resourceVersion`
advances (i.e. a real spec change). Informer churn on bindings does not
trigger recompile; only binding re-attachment. This is the primary cache
and the reason the per-request hot path never calls `env.Compile`.

### 4.6 Param CRDs — secondary informers

[`ensureParamsForPolicyLocked`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L393) spins up a param informer on demand.
If the `ParamKind` resolves via the RESTMapper to a typed resource, it uses
the typed informer; otherwise it creates a dynamic JSON informer. Each
param informer has a cancel func stored in `paramsCRDControllers` and is
stopped in `calculatePolicyData`'s eviction pass when no policy uses it.

### 4.7 From etcd to `hook.Evaluator` — end-to-end

```
  apiserver write → etcd (validatingadmissionpolicy/<name>)
         │                        │
         │                        ▼
         │           (later)  watch event delivered to informer
         │                        │
         │               cache.Store updated (thread-safe)
         │                        │
         │                    notify()
         │                        │
         │         policiesDirty.Store(true)
         │                        │
         │         refreshPolicies() worker tick
         │                        │
         │           calculatePolicyData()
         │                        │
         │           compilePolicyLocked() ── cache miss? ── compilePolicy()
         │                        │                               │
         │                        ▼                               ▼
         │            policies.Store(&newHooks)         cel.NewCompositedCompiler
         │                        │                     env.Compile + env.Program
         ▼                        ▼
  subsequent request ◄── policies.Load() ── hooks visible
```

---

## 5. CEL compilation & evaluation

### 5.1 When expressions are compiled

All CEL compilation happens inside [`validating.compilePolicy`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L147) at
policy refresh time (not at request time). For each policy,
`compilePolicy`:

1. Determines optional-declaration flags from spec:
   ```go
   optionalVars          = {HasParams: policy.Spec.ParamKind != nil, HasAuthorizer: true}
   expressionOptionalVars = {HasParams: same,                         HasAuthorizer: false}
   ```
2. Instantiates one `cel.CompositedCompiler` per policy.
   This adds a policy-specific `variables` map type via one `envSet.Extend`.
3. Compiles and stores `Spec.Variables` expressions into the
   `compositionState.compiledVariables` map. These CEL programs are
   evaluated **lazily** at request time (via `lazy.MapValue`) and cached per
   request so that an unreferenced variable never runs.
4. Compiles the four expression families:
   - `matchConditions`  (filter before validations)
   - `validations[].expression`
   - `validations[].messageExpression`   (used only on failure)
   - `auditAnnotations[].valueExpression`
5. Returns a `validator` (implements the `Validator` interface), which
   retains all four `cel.ConditionEvaluator`s plus the match-conditions
   matcher.

### 5.2 Where AST/program is stored

[`cel/compile.go`](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go#L142-L147):

```go
type CompilationResult struct {
    Program            cel.Program          // executable
    Error              *apiservercel.Error
    ExpressionAccessor ExpressionAccessor   // back-ref to source expr
    OutputType         *cel.Type
}
```

One `CompilationResult` per expression, owned by the `ConditionEvaluator`
(see [cel/condition.go:55-63](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go#L55-L63)):

```go
type condition struct {
    compilationResults []CompilationResult
}
```

The `validator` struct holds these through `ConditionEvaluator` interfaces:

```go
type validator struct {
    celMatcher            matchconditions.Matcher
    validationFilter      cel.ConditionEvaluator
    auditAnnotationFilter cel.ConditionEvaluator
    messageFilter         cel.ConditionEvaluator
    failPolicy            *v1.FailurePolicyType
    compileError          error
}
```

These are retained for the lifetime of the policy's `compiledPolicyEntry`
(i.e. until the next `resourceVersion` change) and referenced by the
`PolicyHook` slice published via `atomic.Pointer`.

### 5.3 Activation — binding input to CEL

At evaluation time, [`cel.newActivation`](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go#L35) builds an
`evaluationActivation`:

```go
va := &evaluationActivation{
    object:                    objectVal,                 // request object
    oldObject:                 oldObjectVal,              // existing object (UPDATE/DELETE)
    params:                    paramsVal,                 // from binding paramRef
    request:                   requestVal.Object,         // admission.AdmissionRequest
    namespace:                 namespaceVal,              // core.v1.Namespace
    authorizer:                authorizerVal,             // library.NewAuthorizerVal
    requestResourceAuthorizer: requestResourceAuthorizerVal,
}
if compositionCtx != nil {
    va.variables = compositionCtx.Variables(va)          // lazy.MapValue
}
```

And `ResolveName` ([activation.go:88-109](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go#L88-L109)) is how cel-go looks up
variables at runtime:

```go
switch name {
case "object":                         return a.object, true
case "oldObject":                      return a.oldObject, true
case "params":                         return a.params, true
case "request":                        return a.request, true
case "namespaceObject":                return a.namespace, true
case "authorizer":                     return a.authorizer, a.authorizer != nil
case "authorizer.requestResource":     return a.requestResourceAuthorizer, ...
case "variables":                      return a.variables, true
}
```

### 5.4 `variables` is lazy per request

[cel/composition.go:196-255](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go#L196-L255):

```go
func (c *compositionContext) Variables(activation any) ref.Val {
    lazyMap := lazy.NewMapValue(c.state.mapType)
    for name, result := range c.state.compiledVariables {
        accessor := &variableAccessor{name: name, result: result, activation: activation, context: c}
        lazyMap.Append(name, accessor.Callback)
    }
    return lazyMap
}
```

`variableAccessor.Callback` runs `result.Program.ContextEval` **the first
time** the variable is referenced in the enclosing expression, then caches
the value in the map. So a policy with 20 variables where only 3 are
touched pays the cost of 3 program executions.

### 5.5 Evaluation — per-expression

[cel/condition.go:90-112](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go#L90-L112) and [cel/activation.go:119+](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go#L119):

```go
func (c *condition) ForInput(ctx, versionedAttr, request, inputs, namespace, budget) ([]EvaluationResult, int64, error) {
    compositionCtx, _ := ctx.(CompositionContext)
    activation, err := newActivation(compositionCtx, versionedAttr, request, inputs, namespace)
    ...
    for i, compilationResult := range c.compilationResults {
        evaluations[i], remainingBudget, err = activation.Evaluate(ctx, compositionCtx, compilationResult, remainingBudget)
        if err != nil { return nil, -1, err }
    }
    return evaluations, remainingBudget, nil
}
```

`activation.Evaluate` calls `program.ContextEval(ctx, activation)` from
cel-go, then:

- Subtracts `details.ActualCost()` from `remainingBudget` (`RuntimeCELCostBudget`).
- Returns `ErrOutOfBudget` if the budget goes negative.
- Returns the value + cost.

### 5.6 The four expression families — where each runs

Inside `validator.Validate` (see [validator.go:85-268](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator.go#L85-L268)):

```
 matchConditions ── celMatcher.Match(ctx, versionedAttr, versionedParams, authz)
        │          short-circuits: if any matchCondition != true, return empty ValidateResult
        ▼
 validations[].expression ── validationFilter.ForInput(...)
        │
        ▼
 messageExpression ── messageFilter.ForInput(...)  [eager today; candidate for lazy]
        │          used only when a validation fails to produce the error message
        ▼
 auditAnnotations ── auditAnnotationFilter.ForInput(...)
                    converts string/null results → audit annotations
```

---

## 6. Request evaluation trace — concrete Deployment

### 6.1 Setup

Policy `no-privileged-pods`:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: no-privileged-pods
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["apps"]
        apiVersions: ["v1"]
        operations: ["CREATE","UPDATE"]
        resources: ["deployments"]
  variables:
    - name: containers
      expression: "object.spec.template.spec.containers"
  matchConditions:
    - name: has-spec
      expression: "has(object.spec.template.spec)"
  validations:
    - expression: "!variables.containers.exists(c, has(c.securityContext) && has(c.securityContext.privileged) && c.securityContext.privileged == true)"
      message:    "privileged containers are not allowed"
      reason:     Invalid
```

Binding:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: no-privileged-pods-binding
spec:
  policyName: no-privileged-pods
  validationActions: ["Deny"]
  matchResources:
    namespaceSelector:
      matchLabels:
        env: prod
```

Request: `POST /apis/apps/v1/namespaces/prod/deployments` body:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: prod}
spec:
  replicas: 3
  template:
    spec:
      containers:
        - {name: nginx, image: nginx:1.25, securityContext: {privileged: true}}
```

### 6.2 Call sequence (pseudo stack)

```
endpoints/handlers/create.go:createHandler
 └─ admission.chainAdmissionHandler.Validate
     └─ validating.Plugin.Validate
         └─ generic.Plugin[PolicyHook].Dispatch
             ├─ isExcludedFromAllHooks(deployments.apps) = false
             ├─ isExcludedFromAPIHooks(deployments.apps) = false
             ├─ WaitForReady() = true
             └─ dispatcher.Dispatch(ctx, a, o, policies.Load())
                 ├─ for hook := "no-privileged-pods":
                 │   ├─ matcher.DefinitionMatches(a, o, policyAcc)
                 │   │     resourceRules: apps/v1 deployments CREATE  → match
                 │   │     namespaceSelector: not set at definition  → skipped
                 │   │     objectSelector:    not set                → skipped
                 │   │     → (matches=true, matchGVR=apps/v1.deployments, matchGVK=apps/v1.Deployment)
                 │   ├─ hook.ConfigurationError == nil
                 │   └─ for binding := "no-privileged-pods-binding":
                 │       ├─ matcher.BindingMatches(a, o, bindingAcc)
                 │       │     namespaceSelector matchLabels{env=prod}
                 │       │       lookup ns "prod" from namespaceInformer.Lister
                 │       │       labels include env=prod → match
                 │       │     → matches=true
                 │       ├─ versionedAttr = admission.NewVersionedAttributes(a, apps/v1.Deployment, o)
                 │       ├─ params = CollectParams(nil, ...) = [nil]    (no paramKind)
                 │       ├─ ns, _ = matcher.GetNamespace(ctx, "prod")  (cached)
                 │       └─ validator.Validate(ctx, matchResource, versionedAttr, nil, ns, 10_000_000, authz)
                 │           ├─ celMatcher.Match(...)
                 │           │     evaluates "has(object.spec.template.spec)"
                 │           │     activation: object = the Deployment
                 │           │     result = true → matches = true
                 │           ├─ validationFilter.ForInput(ctx, ...)
                 │           │     newActivation(..., variables = lazy.MapValue{containers: callback})
                 │           │     evaluate: !variables.containers.exists(c, ...)
                 │           │       → cel-go hits variables.containers ← first touch
                 │           │       → runs compiledVariables["containers"].Program
                 │           │         against activation → returns containers list
                 │           │       → exists(c, ...) iterates 1 container
                 │           │         c.securityContext.privileged == true → true
                 │           │       → outer !(...) → false  ◄── validation FAILS
                 │           ├─ messageFilter.ForInput(...)
                 │           │     no messageExpression configured → no-op
                 │           ├─ decisions = [{
                 │           │     Action: ActionDeny,
                 │           │     Evaluation: EvalDeny,
                 │           │     Reason: Invalid,
                 │           │     Message: "privileged containers are not allowed",
                 │           │     Elapsed: 380µs }]
                 │           └─ auditAnnotationFilter.ForInput(...) → empty
                 ├─ decision.Action == ActionDeny:
                 │     for action in ["Deny"]:
                 │       deniedDecisions = append(..., { ... })
                 │       celmetrics.Metrics.ObserveRejection(...)
                 └─ deniedDecisions has 1 entry:
                     err = admission.NewForbidden(a, <msg>)
                     err.ErrStatus.Reason = Invalid
                     err.ErrStatus.Code = 422
                     err.ErrStatus.Details.Causes = [{ Message: "..." }]
                     return err

 → createHandler sees non-nil error, returns HTTP 422 to client
```

### 6.3 Concrete values flowing through CEL

For the validation expression:

```
Activation:
  object       = {apiVersion:"apps/v1", kind:"Deployment", ...}
  oldObject    = nil                                     (CREATE)
  params       = nil
  request      = {operation:"CREATE", userInfo:{...}, kind:..., resource:..., ...}
  namespaceObject = {metadata:{name:"prod", labels:{env:"prod"}}, ...}
  authorizer   = library.AuthorizerVal (closure over user+authz)
  variables.containers = [{name:"nginx", image:"nginx:1.25",
                           securityContext:{privileged:true}}]

Expression evaluation (AST):
  ![
    .exists(c,
       has(c.securityContext) &&
       has(c.securityContext.privileged) &&
       c.securityContext.privileged == true)
  ](variables.containers)

Step by step (cel-go runtime):
  variables.containers   → triggers callback, returns list (1 item)
  .exists(c, ...)        → iterates; c = nginx container
    has(c.securityContext)               → true
    has(c.securityContext.privileged)    → true
    c.securityContext.privileged == true → true
    exists() short-circuits → true
  outer !(true)          → false

EvalResult: celtypes.Bool(false)
 → in validator.Validate: evalResult.EvalResult != celtypes.True
 → decision.Action = ActionDeny
```

---

## 7. All code paths & edge cases

### 7.1 CREATE vs UPDATE vs DELETE

Driven by `admission.Operation` from `Attributes.GetOperation()` and by the
objects set on `VersionedAttributes`:

| Operation | `VersionedObject` | `VersionedOldObject` | Notes |
|---|---|---|---|
| CREATE | new object | `nil` | `oldObject` resolves to CEL null. |
| UPDATE | new object | existing object | Both available; policies can diff: `object.spec.replicas != oldObject.spec.replicas`. |
| DELETE | `nil` | existing object | `object` is null; policies should reference `oldObject`. |
| CONNECT | new object (subresource) | `nil` | Rare for VAP; only fires if `matchConstraints` opts in. |

[admission/interfaces.go:53-56](staging/src/k8s.io/apiserver/pkg/admission/interfaces.go#L53-L56) spells out the contract; `objectToResolveVal`
in [cel/condition.go:76-85](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go#L76-L85) returns `nil` for nil inputs, which cel-go
surfaces to expressions as `null`.

### 7.2 Missing fields

CEL uses `has()` for field-presence checks (structural, not value-truthiness).
Accessing a missing field without `has()` evaluates to `null`, and attempting
to index through it raises `no_such_field` errors. `failurePolicy` governs
how the error is treated:

- `Fail`  → Deny with `EvalError` message "no such key: X".
- `Ignore` → Admit despite the error (error still recorded for audit).

Pattern used by SIG Auth docs:

```
has(object.spec.replicas) && object.spec.replicas > 0
```

### 7.3 Multiple policies & bindings

- Policies evaluate in informer iteration order (not deterministic across
  restarts), but all matching policies run; the dispatcher collects
  **all** denials before returning.
- Only the **first** denial populates `StatusError.Message` and `Reason`;
  others are appended as `Details.Causes[]`. Clients read `Message` by
  default but `kubectl` surfaces causes too.
- A single binding with a multi-match selector → evaluates per param.
  Denials short-circuit nothing here either; every param is evaluated.

### 7.4 FailurePolicy — Fail vs Ignore

Two layers of `FailurePolicy` apply:

1. **Top-level policy error** (config-level: paramRef broken, paramKind
   unresolved, compile error).
   - Handled in [generic/policy_dispatcher.go:207-226](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_dispatcher.go#L207-L226)
     (generic) and [validating/dispatcher.go:86-114](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L86-L114) (validating).
2. **Per-evaluation error** (CEL runtime error, cost-budget exceeded).
   - Handled in [validator.go:60-77](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator.go#L60-L77):
     ```go
     func policyDecisionActionForError(f v1.FailurePolicyType) PolicyDecisionAction {
         if f == v1.Ignore { return ActionAdmit }
         return ActionDeny
     }
     ```

For `Fail` mode the error becomes a 422/403 to the client; for `Ignore`
mode the policy is silently skipped (metrics do still record it — see
[`celmetrics.Metrics.ObserveAdmission`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L231)).

### 7.5 CEL execution errors, in detail

Three error classes reach `validator.Validate`:

| Error | Where detected | Handling |
|---|---|---|
| `apiservercel.ErrInternal` | `activation.Evaluate` via cel-go runtime | Honors `failurePolicy`; emits `EvalError`. |
| `apiservercel.ErrOutOfBudget` | Cost budget exhausted (`RuntimeCELCostBudget = 10_000_000` by default in [staging/src/k8s.io/apiserver/pkg/apis/cel/config.go](staging/src/k8s.io/apiserver/pkg/apis/cel/config.go)) | Same as above. |
| `evalResult.Error` | Expression-level error captured in `EvaluationResult` | Per-decision; decision-level `Evaluation=EvalError`. |

Validator.Validate routes message-expression errors separately
([validator.go:163-166](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator.go#L163-L166)): the validation decision itself inherits the failure
policy when the message expression errored internally.

### 7.6 Subresources

`matchConstraints.resources` must explicitly list the subresource suffix
(`deployments/status`). Subresources have their own GVK and their own
`matchPolicy` resolution; `matching.go`'s `matchesResourceRules` consults
`o.GetEquivalentResourceMapper()` for `matchPolicy: Equivalent`.

### 7.7 paramKind edge cases

- `paramKind` set, binding has no `paramRef`: param is `nil`; CEL
  expressions referencing `params.*` see null.
- `paramKind` set, `paramRef.name` specified, param missing in cache:
  if `parameterNotFoundAction: Deny`, the dispatcher records a config
  error → FailurePolicy path. If `Allow`, the binding is skipped silently.
- `paramKind` namespaced, binding matches cluster-scoped resource, and
  `paramRef.namespace` not set → config error ("cannot use namespaced
  paramRef in policy binding that matches cluster-scoped resources"),
  see [generic/policy_dispatcher.go:296](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_dispatcher.go#L296).

### 7.8 DryRun

Requests with `?dryRun=All` still run admission; `Attributes.IsDryRun()` is
exposed to CEL via `request.dryRun`. Policies can short-circuit on dry-run
if desired; the apiserver already handles not-persisting.

### 7.9 Readiness gate

If informers haven't synced yet,
[generic/plugin.go:330-332](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/plugin.go#L330-L332) returns
`admission.NewForbidden(a, "not yet ready to handle request")`. This is a
hard-deny that clients see as 403; it protects against a newly-booted
apiserver silently admitting requests that should have been denied by
policies that haven't loaded yet.

---

## 8. Performance & memory considerations

### 8.1 Where memory is retained

Per policy, at steady state:

```
compiledPolicyEntry                     ~few hundred bytes
└── Validator (validator struct)        ~100 B + pointers
    ├── CompositedCompiler/state         ~25 KB  (policy-specific variable mapType + envSet)
    ├── validationFilter.compilationResults[]   N × ~30–50 KB
    ├── matchFilter (via matchconditions.Matcher) N × ~30–50 KB
    ├── messageFilter.compilationResults[]      M × ~30–50 KB
    └── auditAnnotationFilter.compilationResults[] K × ~30–50 KB
```

`cel.Program` is the dominant retained object. It holds:

- The checked expression AST.
- An interpretable program ("eval plan").
- Captured references to the environment's type registry and function
  libraries.

### 8.2 Where CPU is spent

Compile path (once per policy `resourceVersion` change):

1. `env.Extend(...)` — O(# declarations + chain depth). The most expensive
   step. Issue kubernetes/kubernetes#131417 pinpointed this as dominant
   before the lazy-envSet fix in [cel/compile.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go).
2. `env.Compile(expr)` — CEL parser + type-checker on the AST.
3. `env.Program(ast)` — lowering to evaluation plan; applies
   `cel.InterruptCheckFrequency(CheckFrequency)`.

Request path (per admission request):

1. Namespace lister hit — O(1).
2. Match evaluation — O(# policies) with early exit on namespaceSelector /
   objectSelector / resourceRules.
3. Object conversion (`NewVersionedAttributes`) — runs `runtime.Scheme`
   conversion; the cache in `versionedAttributeAccessor` keeps this to
   "once per distinct GVK per request."
4. Activation build — allocates `evaluationActivation` (small struct) and
   lazy `variables` map.
5. CEL evaluation — `program.ContextEval`. Cost is bounded by
   `RuntimeCELCostBudget`.
6. Audit/warning/deny bookkeeping — negligible.

### 8.3 Caching layers today

| Cache | Scope | Key | Lifetime |
|---|---|---|---|
| `policySource.policies` | process | — | replaced atomically every refresh |
| `policySource.compiledPolicies` | process | `(namespace, name)` | invalidated when `resourceVersion` changes |
| `versionedAttributeAccessor.versionedAttrs` | request | `GroupVersionKind` | GC'd after request |
| `compositionState.compiledVariables` | policy | variable name | invalidated with policy |
| `lazy.MapValue` (variables) | request | variable name | GC'd after request |
| `compiler.varEnvs` (post-fix) | policy | `OptionalVariableDeclarations` | invalidated with policy |

There is **no** expression-level dedup across policies today (see
[plugin-optimization-analysis.md](plugin-optimization-analysis.md)).

### 8.4 Known bottlenecks and levers

| Bottleneck | Lever |
|---|---|
| Per-policy env setup (#131417) | Lazy envSet in `cel/compile.go` (landed above). |
| Expression duplication across policies | Process-wide program cache keyed by `(expr, options, envType, varSig)` — proposed in the optimization analysis doc. |
| `matchConditions` evaluated after `DefinitionMatches` + `BindingMatches` even if cheap labels would reject | Ordered as-is because matchConditions may access `authorizer`, which is a request-side dep. Reorder is intentional. |
| `namespaceInformer` cache miss | If lister is stale, `GetNamespace` falls back to direct API GET — under heavy policy churn, this can add latency. |
| Single mutex on `policySource.lock` during refresh | Under very high binding churn this can stall `Hooks()` readers briefly; the atomic publish keeps the hot path lock-free. |

### 8.5 Cost budget

`celconfig.RuntimeCELCostBudget` (default `10_000_000` units) is decremented
across all expression evaluations in a single `validator.Validate` call:

```
remainingBudget = 10_000_000
remainingBudget -= cost(matchCondition[0..N])
remainingBudget -= cost(validation[0..M])
remainingBudget -= cost(messageExpression[0..M])
remainingBudget = RuntimeCELCostBudget (reset)   ← auditAnnotations get fresh budget
```

Running out returns `ErrOutOfBudget` per §7.5.

---

## 9. Final diagram + summary

### 9.1 Clean architecture view

```
 ┌─────────────────────── KUBE-APISERVER PROCESS ──────────────────────────┐
 │                                                                         │
 │   ┌────────────────┐  ┌────────────────┐                                │
 │   │  REST pipeline │  │ Informers      │                                │
 │   │  (handlers/)   │  │ ┌─────────┐    │                                │
 │   │                │  │ │VAP list │◄──┐│                                │
 │   │ authN/authZ    │  │ │VAPB list│◄─┐││                                │
 │   │ decode         │  │ │NS list  │◄┐│││                                │
 │   │ mutAdmission   │  │ └─────────┘ ││││                                │
 │   │ defaulting     │  └─────┬───────┼┼┼┘                                │
 │   │ validation     │        │ watch ││└── namespace label lookups        │
 │   │ valAdmission ──┼────────┤       │└─── binding resourceVersion stream │
 │   │ storage -> etcd│        │       └──── policy resourceVersion stream  │
 │   └────────────────┘        │                                           │
 │                             ▼                                           │
 │     ┌───────────────────────────────────────────────────────────┐       │
 │     │            generic.policySource                           │       │
 │     │   ┌────────────┐   ┌─────────────────────────┐            │       │
 │     │   │  notify    │──▶│ refreshPolicies() (1 Hz)│            │       │
 │     │   └────────────┘   └───────────┬─────────────┘            │       │
 │     │                                ▼                          │       │
 │     │          calculatePolicyData() + compile cache            │       │
 │     │          (keyed by resourceVersion)                       │       │
 │     │                                │                          │       │
 │     │                                ▼                          │       │
 │     │          atomic.Pointer[[]PolicyHook]                     │       │
 │     └────────────────────────────────┬──────────────────────────┘       │
 │                                      │                                  │
 │          valAdmission path ──▶ generic.Plugin.Dispatch                  │
 │                                      │                                  │
 │                                      ▼                                  │
 │     ┌───────────────────────────────────────────────────────────┐       │
 │     │         validating.dispatcher                             │       │
 │     │  for each policy hook visible via policies.Load():        │       │
 │     │    matcher.DefinitionMatches                              │       │
 │     │    for each binding:                                      │       │
 │     │      matcher.BindingMatches                               │       │
 │     │      CollectParams (param informer lister)                │       │
 │     │      NewVersionedAttributes (cache per GVK)               │       │
 │     │      validator.Validate:                                  │       │
 │     │        matchConditions  ─ CEL program ─▶ Bool             │       │
 │     │        validations      ─ CEL program ─▶ Bool per expr    │       │
 │     │        messageExpr      ─ CEL program ─▶ String (on fail) │       │
 │     │        auditAnnots      ─ CEL program ─▶ String / null    │       │
 │     │  compose deniedDecisions into k8s StatusError             │       │
 │     └───────────────────────────────────────────────────────────┘       │
 │                                      │                                  │
 │                                      ▼                                  │
 │                          Admission chain returns (err or nil)           │
 │                                                                         │
 └─────────────────────────────────────────────────────────────────────────┘
```

### 9.2 Bullet summary

**Startup**
- `validating.Register` adds the plugin factory; `validating.NewPlugin`
  wires a `generic.Plugin[PolicyHook]` with
  [NewPolicySource](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L108) and [NewDispatcher](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L49).
- `ValidateInitialization` starts a goroutine running `source.Run`,
  which launches the 1-Hz refresh loop after informer sync.

**Steady state**
- Informer events on VAP/VAPB/Namespace mark `policiesDirty=true`.
- `refreshPolicies` rebuilds `PolicyHook`s and publishes them atomically.
- `compilePolicyLocked` short-circuits recompile on matching
  `resourceVersion` — CEL compile happens only on real policy changes.

**Per-request**
- `generic.Plugin.Dispatch` filters out reviews and
  admissionConfigResources, gates on readiness, passes `source.Hooks()`
  to the validating dispatcher.
- Dispatcher matches definition and binding, fetches params and namespace,
  converts object to the matched GVK once, then invokes
  `validator.Validate` which runs the four CEL pipelines.
- Denials are collected, filtered by `binding.Spec.ValidationActions`
  (`Deny`, `Warn`, `Audit`) and `FailurePolicy`, then folded into a single
  `StatusError` with `Causes[]`.

**CEL**
- Compile happens in `validating.compilePolicy` via
  `cel.NewCompositedCompiler` → `compiler.CompileCELExpression`.
- Environments are built lazily per `OptionalVariableDeclarations`
  combination (post-#131417 fix).
- At request time, `evaluationActivation` binds `object`, `oldObject`,
  `params`, `request`, `namespaceObject`, `authorizer`, and a lazy
  `variables` map. `program.ContextEval` runs under a cost budget.

**Resilience**
- Config errors flow through `FailurePolicy` (Fail/Ignore).
- CEL runtime errors likewise.
- Readiness short-circuits admission before the informer cache is warm.
- Admission-config resources are specifically excluded from API-backed
  policy evaluation to prevent self-dependency.
