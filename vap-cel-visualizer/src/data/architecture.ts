export const vapYaml = `apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: require-prod-test
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      operations: ["CREATE", "UPDATE"]
      resources: ["pods"]
  validations:
  - expression: 'object.metadata.labels.environment == "prod" && object.metadata.name == "test"'
    message: 'Pod must be named test and labeled environment=prod.'
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: require-prod-test-binding
spec:
  policyName: require-prod-test
  validationActions: [Deny]`;

export const admissionFlowNodes = [
  { id: 'kubectl', label: 'kubectl apply', detail: 'YAML becomes an HTTP request to kube-apiserver.', x: 0, y: 80 },
  { id: 'http', label: 'kube-apiserver HTTP', detail: 'Authentication, authorization, then admission.', x: 220, y: 80 },
  { id: 'mutating', label: 'Mutating admission', detail: 'Mutating plugins run first and can change the object.', x: 460, y: 10 },
  { id: 'validating', label: 'Validating admission', detail: 'Validating plugins run after mutation.', x: 460, y: 150 },
  { id: 'vap', label: 'VAP plugin', detail: 'Policies and bindings are matched against attributes.', x: 720, y: 150 },
  { id: 'cache', label: 'Compiled CEL cache', detail: 'PolicyHook points to Validator and cel.Program objects.', x: 980, y: 30 },
  { id: 'activation', label: 'Activation', detail: 'object, oldObject, request, params, namespaceObject.', x: 980, y: 150 },
  { id: 'eval', label: 'CEL interpreter', detail: 'Interpretable tree walks attributes and calls overloads.', x: 1220, y: 150 },
  { id: 'decision', label: 'allow / deny', detail: 'False result can deny, warn, or audit by binding action.', x: 1460, y: 150 },
];

export const admissionFlowEdges = [
  ['kubectl', 'http'],
  ['http', 'mutating'],
  ['mutating', 'validating'],
  ['http', 'validating'],
  ['validating', 'vap'],
  ['vap', 'cache'],
  ['vap', 'activation'],
  ['cache', 'eval'],
  ['activation', 'eval'],
  ['eval', 'decision'],
];

export const compileStages = [
  {
    name: 'Expression String',
    artifact: 'validation.expression',
    persisted: false,
    detail: 'The policy spec stores the CEL source text. Kubernetes normalizes it for cache keys.',
    cost: 'cheap',
  },
  {
    name: 'Tokens',
    artifact: 'lexer tokens',
    persisted: false,
    detail: 'cel-go parser scans identifiers, literals, operators, member selections, and macros.',
    cost: 'transient allocation',
  },
  {
    name: 'Parser',
    artifact: 'unchecked AST',
    persisted: false,
    detail: 'The parser creates common/ast.Expr nodes with ids and source information.',
    cost: 'compile-time',
  },
  {
    name: 'Type Checker',
    artifact: 'checked AST',
    persisted: true,
    detail: 'The checker resolves declarations, overload ids, type maps, and reference maps.',
    cost: 'expensive',
  },
  {
    name: 'Planner',
    artifact: 'Interpretable tree',
    persisted: true,
    detail: 'env.Program creates an interpreter and plans AST nodes into evalAnd, evalEq, evalAttr, and call nodes.',
    cost: 'expensive',
  },
  {
    name: 'Program',
    artifact: 'cel.Program',
    persisted: true,
    detail: 'Kubernetes stores this in ConditionEvaluator and may also retain it in the process-wide compile cache.',
    cost: 'reuse across requests',
  },
];

export const memoryObjects = [
  {
    name: 'Base EnvSet',
    owner: 'Kubernetes CEL environment package',
    phase: 'persistent',
    stores: 'libraries, declarations, cost options, compatibility version',
    allocation: 'Built once per compatibility version; Extend is intentionally avoided at request time.',
    reuse: 'process-wide',
  },
  {
    name: 'cel.Env',
    owner: 'cel-go',
    phase: 'persistent',
    stores: 'parser, checker config, declarations, macros, program options, shared dispatcher',
    allocation: 'Environment construction is compile-time only. The shared dispatcher cache saves repeated program memory.',
    reuse: 'per env template / optional variable set',
  },
  {
    name: 'AST',
    owner: 'cel-go common/ast',
    phase: 'compile',
    stores: 'root expr, source info, type map, reference map',
    allocation: 'Created by parser and checker. Checked metadata feeds overload resolution.',
    reuse: 'used to create Program, then not needed on the admission hot path',
  },
  {
    name: 'cel.Program',
    owner: 'cel-go program',
    phase: 'persistent',
    stores: 'dispatcher, interpreter, interpretable tree, attribute factory, cost estimator',
    allocation: 'Kubernetes comments estimate roughly 150-250 KB retained per cached program.',
    reuse: 'per compiled expression until policy changes or cache evicts',
  },
  {
    name: 'Validator',
    owner: 'VAP validating plugin',
    phase: 'persistent',
    stores: 'match conditions, validation filters, audit filters, message expressions',
    allocation: 'Rebuilt when the policy resourceVersion changes.',
    reuse: 'all matching requests',
  },
  {
    name: 'PolicyHook',
    owner: 'generic policy source',
    phase: 'persistent',
    stores: 'policy, bindings, param informer, evaluator, configuration error',
    allocation: 'Stored in an atomic slice that the dispatcher reads lock-free.',
    reuse: 'all admission requests between source refreshes',
  },
  {
    name: 'VersionedAttributes',
    owner: 'dispatcher / admission plugin',
    phase: 'request',
    stores: 'converted object for the policy match version',
    allocation: 'Deferred until policy, binding, and params match; conversion can be costly.',
    reuse: 'request-local',
  },
  {
    name: 'evaluationActivation',
    owner: 'Kubernetes CEL admission plugin',
    phase: 'request',
    stores: 'object, oldObject, params, request, namespaceObject, authorizer, variables',
    allocation: 'Created for each validation input.',
    reuse: 'request-local',
  },
  {
    name: 'ctxActivation',
    owner: 'cel-go program',
    phase: 'pooled',
    stores: 'activation plus context cancellation check',
    allocation: 'Borrowed from an internal pool around ContextEval.',
    reuse: 'pooled',
  },
  {
    name: 'Attribute qualifiers',
    owner: 'cel-go interpreter',
    phase: 'persistent',
    stores: 'absolute name plus qualifier chain for field/map access',
    allocation: 'Planned into the interpretable tree for repeated use.',
    reuse: 'per program',
  },
];

export const dispatcherRows = [
  {
    syntax: 'a && b',
    overload: '_&&_',
    celGoPath: 'planner.planCallLogicalAnd -> evalAnd',
    dispatch: 'Special-cased, not a normal runtime table lookup.',
    trait: 'bool truthiness with unknown/error propagation',
  },
  {
    syntax: 'a == b',
    overload: '_==_',
    celGoPath: 'planner.planCallEqual -> evalEq',
    dispatch: 'Special-cased equality calls types.Equal.',
    trait: 'ref.Val Equal semantics',
  },
  {
    syntax: 's.startsWith("x")',
    overload: 'starts_with_string_string',
    celGoPath: 'resolveFunction -> dispatcher.FindOverload -> evalBinary',
    dispatch: 'Lookup by overload id from checked AST ref map.',
    trait: 'ReceiverType / String operations',
  },
  {
    syntax: 's.contains("x")',
    overload: 'contains_string_string',
    celGoPath: 'resolveFunction -> dispatcher.FindOverload -> evalBinary',
    dispatch: 'Lookup by overload id from checked AST ref map.',
    trait: 'ReceiverType / String operations',
  },
  {
    syntax: 'object.metadata.labels.environment',
    overload: 'select / qualifier chain',
    celGoPath: 'planner.planSelect -> evalAttr -> Attribute.Resolve',
    dispatch: 'No function dispatch for ordinary field selection after planning.',
    trait: 'MapperType, IndexerType, field qualifiers',
  },
];

export const variableRows = [
  {
    name: 'object',
    source: 'Admission object, converted to policy match version',
    lifetime: 'request',
    note: 'For CREATE/UPDATE this is the new object. Delete can have nil object depending on admission attributes.',
  },
  {
    name: 'oldObject',
    source: 'Existing object before update/delete',
    lifetime: 'request',
    note: 'Converted through the same object-to-resolve-value path.',
  },
  {
    name: 'request',
    source: 'admission.k8s.io AdmissionRequest view',
    lifetime: 'request',
    note: 'Contains operation, userInfo, resource, subresource, namespace, dryRun, and options.',
  },
  {
    name: 'params',
    source: 'Parameter resource selected by binding',
    lifetime: 'request',
    note: 'Optional. Absent params are handled according to parameterNotFoundAction and failurePolicy.',
  },
  {
    name: 'namespaceObject',
    source: 'Namespace lister',
    lifetime: 'request',
    note: 'Fetched only when needed for namespaced requests and exposed to CEL as namespaceObject.',
  },
  {
    name: 'authorizer',
    source: 'Authorizer wrapper',
    lifetime: 'request',
    note: 'Available for policy expressions where Kubernetes enables authorizer variables.',
  },
];

export const sourceMap = [
  {
    concept: 'Admission chain execution',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/chain.go:30',
    detail: 'Mutating handlers run through Admit; validating handlers run through Validate and stop on error.',
  },
  {
    concept: 'VAP plugin creation',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go:109',
    detail: 'Registers the handler for create/update/delete/connect and wires source and dispatcher factories.',
  },
  {
    concept: 'Policy compilation',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go:147',
    detail: 'Builds Kubernetes CEL compilers, variables, match conditions, validations, audits, and messages.',
  },
  {
    concept: 'Policy source cache',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go:52',
    detail: 'Holds compiled policies, dirty flags, atomic policy list, informers, and param informers.',
  },
  {
    concept: 'Compiled evaluator reuse',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go:470',
    detail: 'Reuses compiled evaluators by namespaced policy name and resourceVersion.',
  },
  {
    concept: 'Request dispatch',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go:71',
    detail: 'Matches policy, binding, params, namespace, then calls hook.Evaluator.Validate.',
  },
  {
    concept: 'Validation result handling',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator.go:85',
    detail: 'Evaluates match conditions, validation filters, message expressions, and audit annotations.',
  },
  {
    concept: 'Kubernetes CEL compile cache',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_cache.go:39',
    detail: 'Process-wide LRU. Comments document cache size and retained program memory estimates.',
  },
  {
    concept: 'Compile fresh / env.Program',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go:304',
    detail: 'Runs env.Compile, validates output type, and creates cel.Program with cost/interruption options.',
  },
  {
    concept: 'Activation creation',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go:35',
    detail: 'Builds evaluationActivation from object, oldObject, params, request, namespace, and authorizer.',
  },
  {
    concept: 'Condition evaluation',
    file: 'staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go:87',
    detail: 'Creates activation, evaluates each compiled program, and tracks remaining cost budget.',
  },
  {
    concept: 'cel.Env structure',
    file: 'vendor/github.com/google/cel-go/cel/env.go:129',
    detail: 'Owns parser, checker, declarations, macros, program options, and shared dispatcher cache.',
  },
  {
    concept: 'cel-go shared dispatcher',
    file: 'vendor/github.com/google/cel-go/cel/env.go:170',
    detail: 'Builds function overload dispatcher once per environment to reduce per-program retained memory.',
  },
  {
    concept: 'Program creation',
    file: 'vendor/github.com/google/cel-go/cel/program.go:172',
    detail: 'Creates dispatcher, attribute factory, interpreter, planner options, and the interpretable tree.',
  },
  {
    concept: 'ContextEval',
    file: 'vendor/github.com/google/cel-go/cel/program.go:331',
    detail: 'Wraps an Activation for context cancellation, then evaluates through the program.',
  },
  {
    concept: 'Planner call lowering',
    file: 'vendor/github.com/google/cel-go/interpreter/planner.go:214',
    detail: 'Lowers calls into special forms or dispatcher-backed eval nodes.',
  },
  {
    concept: 'Short-circuit AND',
    file: 'vendor/github.com/google/cel-go/interpreter/interpretable.go:286',
    detail: 'evalAnd evaluates the left branch first and can skip the right branch.',
  },
  {
    concept: 'Attribute resolution',
    file: 'vendor/github.com/google/cel-go/interpreter/attributes.go:300',
    detail: 'AbsoluteAttribute resolves the variable name, then applies field/map qualifiers.',
  },
  {
    concept: 'Checker overload resolution',
    file: 'vendor/github.com/google/cel-go/checker/checker.go:297',
    detail: 'Filters candidates by call style and assignability, then records overload ids and result type.',
  },
  {
    concept: 'Standard function declarations',
    file: 'vendor/github.com/google/cel-go/common/stdlib/standard.go:45',
    detail: 'Declares logical operators, equality, strings.contains, startsWith, and many other overloads.',
  },
];

export const mermaidCompile = `flowchart LR
  expr["Expression source"] --> parse["parser.Parse"]
  parse --> unchecked["unchecked AST"]
  unchecked --> check["checker.Check"]
  check --> checked["checked AST<br/>typeMap + refMap"]
  checked --> program["env.Program"]
  program --> planner["interpreter planner"]
  planner --> tree["Interpretable tree<br/>evalAnd -> evalEq -> evalAttr"]
  tree --> cache["Kubernetes compile cache / Validator"]
`;

export const mermaidMemory = `flowchart TB
  subgraph Compile_Time["compile-time"]
    src["expression string"]
    ast["AST + source info"]
    checked["checked AST<br/>overload ids"]
  end
  subgraph Persistent["persistent across requests"]
    env["EnvSet / cel.Env"]
    prog["cel.Program"]
    interp["Interpretable nodes"]
    disp["shared dispatcher"]
    hook["PolicyHook + Validator"]
  end
  subgraph Request_Time["request-time"]
    attr["admission.Attributes"]
    conv["VersionedAttributes"]
    act["evaluationActivation"]
    details["EvalDetails + cost"]
  end
  src --> ast --> checked --> prog
  env --> prog
  disp --> prog
  prog --> interp
  prog --> hook
  attr --> conv --> act
  hook --> act
  act --> interp
  interp --> details
`;
