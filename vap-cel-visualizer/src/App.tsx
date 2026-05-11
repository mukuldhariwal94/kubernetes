import { useEffect, useMemo, useRef, useState } from 'react';
import Editor from '@monaco-editor/react';
import type { Monaco } from '@monaco-editor/react';
import { Controls, MarkerType, MiniMap, ReactFlow, type Edge, type Node } from '@xyflow/react';
import mermaid from 'mermaid';
import { AnimatePresence, motion } from 'framer-motion';
import {
  Activity,
  ArrowRight,
  Binary,
  Boxes,
  Braces,
  Bug,
  CheckCircle2,
  CircuitBoard,
  Clock3,
  Code2,
  Database,
  Eye,
  FileCode2,
  GitBranch,
  Layers3,
  MemoryStick,
  Play,
  RefreshCw,
  Search,
  ShieldCheck,
  Split,
  TableProperties,
  TimerReset,
  Workflow,
  XCircle,
  Zap,
} from 'lucide-react';
import { Badge } from './components/ui/badge';
import { Button } from './components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './components/ui/card';
import { Tabs } from './components/ui/tabs';
import {
  admissionFlowEdges,
  admissionFlowNodes,
  compileStages,
  dispatcherRows,
  memoryObjects,
  mermaidCompile,
  mermaidMemory,
  sourceMap,
  vapYaml,
  variableRows,
} from './data/architecture';
import {
  describeNode,
  evaluateCel,
  flattenAst,
  nodeKindTone,
  sampleExpression,
  type AllocationRow,
  type CelNode,
  type EvaluationOutput,
  type EvaluationStep,
} from './lib/celSimulator';
import { cn } from './lib/utils';

const navItems = [
  ['pipeline', 'Pipeline'],
  ['architecture', 'VAP'],
  ['compile', 'Compile'],
  ['memory', 'Memory'],
  ['runtime', 'Runtime'],
  ['dispatch', 'Dispatch'],
  ['activation', 'Activation'],
  ['simulator', 'Simulator'],
  ['source', 'Source'],
];

function App() {
  const [expression, setExpression] = useState(sampleExpression);
  const [environment, setEnvironment] = useState('prod');
  const [name, setName] = useState('test');

  const evaluation = useMemo(() => {
    try {
      return { data: evaluateCel(expression, { environment, name, namespace: 'default' }), error: null };
    } catch (error) {
      return { data: null, error: error instanceof Error ? error.message : String(error) };
    }
  }, [expression, environment, name]);

  return (
    <main className="min-h-screen">
      <Header evaluation={evaluation.data} />
      <StickyNav />
      <div className="mx-auto flex w-full max-w-[1500px] flex-col gap-8 px-4 pb-16 pt-6 sm:px-6 lg:px-8">
        <PipelineSection />
        <ArchitectureSection />
        <CompileSection expression={expression} setExpression={setExpression} evaluation={evaluation.data} error={evaluation.error} />
        <MemorySection evaluation={evaluation.data} />
        <RuntimeSection evaluation={evaluation.data} />
        <DispatcherSection />
        <ActivationSection />
        <SimulatorSection
          expression={expression}
          setExpression={setExpression}
          environment={environment}
          setEnvironment={setEnvironment}
          name={name}
          setName={setName}
          evaluation={evaluation.data}
          error={evaluation.error}
        />
        <SourceSection />
      </div>
    </main>
  );
}

function Header({ evaluation }: { evaluation: EvaluationOutput | null }) {
  return (
    <section className="border-b border-line bg-ink/85 backdrop-blur">
      <div className="mx-auto grid w-full max-w-[1500px] gap-6 px-4 py-8 sm:px-6 lg:grid-cols-[1.3fr_0.7fr] lg:px-8">
        <div className="space-y-5">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone="cyan">Kubernetes ValidatingAdmissionPolicy</Badge>
            <Badge tone="green">cel-go runtime visualizer</Badge>
            <Badge tone="amber">request-time debugger</Badge>
          </div>
          <div className="max-w-5xl space-y-3">
            <h1 className="text-3xl font-semibold tracking-normal text-white sm:text-5xl">
              Watch a VAP expression become a cached program, then execute inside admission.
            </h1>
            <p className="max-w-4xl text-base leading-7 text-slate-300">
              This app follows one Kubernetes CEL validation from YAML, through policy compilation and cache reuse, into activation creation,
              attribute resolution, overload dispatch, AST walking, cost tracking, and the final allow/deny decision.
            </p>
          </div>
          <div className="rounded-lg border border-line bg-panel p-3 font-mono text-sm text-cyan-100 shadow-glow">
            object.metadata.labels.environment == "prod" && object.metadata.name == "test"
          </div>
          <div className="grid gap-3 sm:grid-cols-3">
            <Metric icon={ShieldCheck} label="current decision" value={evaluation?.allowed ? 'ALLOW' : 'DENY'} tone={evaluation?.allowed ? 'green' : 'rose'} />
            <Metric icon={MemoryStick} label="request estimate" value={evaluation ? `${evaluation.estimates.requestAllocBytes} B` : 'invalid'} tone="amber" />
            <Metric icon={Workflow} label="interpreter steps" value={evaluation ? String(evaluation.estimates.steps) : 'invalid'} tone="cyan" />
          </div>
        </div>
        <Card className="self-end">
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Braces className="h-4 w-4 text-aqua" />
              Why this CEL syntax is correct
            </CardTitle>
            <CardDescription>Valid Kubernetes CEL field selection, with one caveat for label keys.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3 text-sm leading-6 text-slate-300">
            <p>
              Kubernetes declares <code>object</code> as a CEL variable for the admission object. The segments after it are select
              qualifiers over structured fields and maps.
            </p>
            <p>
              <code>environment</code> is a valid identifier, so <code>object.metadata.labels.environment</code> is readable. For label keys
              containing <code>/</code>, <code>.</code>, or <code>-</code>, use bracket access such as{' '}
              <code>object.metadata.labels["app.kubernetes.io/name"]</code>.
            </p>
            <p>
              CEL uses double-quoted strings, <code>==</code> for equality, and <code>&amp;&amp;</code> for boolean conjunction.
            </p>
          </CardContent>
        </Card>
      </div>
    </section>
  );
}

function StickyNav() {
  return (
    <div className="sticky top-0 z-30 border-b border-line bg-ink/90 backdrop-blur">
      <nav className="mx-auto flex w-full max-w-[1500px] gap-1 overflow-x-auto px-4 py-2 sm:px-6 lg:px-8">
        {navItems.map(([href, label]) => (
          <a
            key={href}
            href={`#${href}`}
            className="rounded px-3 py-2 text-xs font-medium text-slate-400 transition-colors hover:bg-slate-800 hover:text-white"
          >
            {label}
          </a>
        ))}
      </nav>
    </div>
  );
}

function Section({
  id,
  eyebrow,
  title,
  description,
  icon: Icon,
  children,
}: {
  id: string;
  eyebrow: string;
  title: string;
  description: string;
  icon: typeof Workflow;
  children: React.ReactNode;
}) {
  return (
    <section id={id} className="scroll-mt-20 space-y-4">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
        <div className="max-w-4xl space-y-2">
          <Badge tone="slate" className="gap-1">
            <Icon className="h-3.5 w-3.5" />
            {eyebrow}
          </Badge>
          <h2 className="text-2xl font-semibold tracking-normal text-white sm:text-3xl">{title}</h2>
          <p className="text-sm leading-6 text-slate-400 sm:text-base">{description}</p>
        </div>
      </div>
      {children}
    </section>
  );
}

function Metric({
  icon: Icon,
  label,
  value,
  tone,
}: {
  icon: typeof Workflow;
  label: string;
  value: string;
  tone: 'cyan' | 'green' | 'amber' | 'rose';
}) {
  const toneClass = {
    cyan: 'text-aqua',
    green: 'text-mint',
    amber: 'text-amber',
    rose: 'text-rose',
  }[tone];
  return (
    <div className="rounded-lg border border-line bg-panel2 p-3">
      <div className="flex items-center gap-2 text-xs uppercase text-slate-500">
        <Icon className={cn('h-4 w-4', toneClass)} />
        {label}
      </div>
      <div className={cn('mt-2 font-mono text-xl font-semibold', toneClass)}>{value}</div>
    </div>
  );
}

function PipelineSection() {
  const nodes: Node[] = admissionFlowNodes.map((node) => ({
    id: node.id,
    position: { x: node.x, y: node.y },
    data: {
      label: (
        <div className="space-y-1">
          <div className="text-sm font-semibold text-white">{node.label}</div>
          <div className="max-w-[170px] text-xs leading-5 text-slate-400">{node.detail}</div>
        </div>
      ),
    },
    style: {
      width: 190,
      background: node.id === 'vap' ? '#123022' : '#121925',
      border: node.id === 'decision' ? '1px solid #4ade80' : '1px solid #273243',
      color: '#e2e8f0',
      padding: 10,
    },
  }));
  const edges: Edge[] = admissionFlowEdges.map(([source, target], index) => ({
    id: `${source}-${target}`,
    source,
    target,
    animated: index > 5,
    markerEnd: { type: MarkerType.ArrowClosed, color: '#22d3ee' },
    style: { stroke: index > 5 ? '#22d3ee' : '#64748b', strokeWidth: 2 },
  }));

  return (
    <Section
      id="pipeline"
      eyebrow="Core visual flow"
      title="Admission Request Traversal"
      description="A request hits kube-apiserver, moves through mutating and validating admission, then the VAP plugin combines cached policy state with per-request activation data."
      icon={Workflow}
    >
      <div className="grid gap-4 lg:grid-cols-[1fr_360px]">
        <Card className="h-[430px] overflow-hidden">
          <ReactFlow nodes={nodes} edges={edges} fitView minZoom={0.35} maxZoom={1.2}>
            <MiniMap pannable zoomable nodeColor={(node) => (node.id === 'vap' ? '#4ade80' : '#22d3ee')} />
            <Controls />
          </ReactFlow>
        </Card>
        <div className="grid gap-3">
          {[
            ['1', 'Admission chain', 'Mutating handlers can change the object; validating handlers observe the final version.'],
            ['2', 'Policy matching', 'Policy matchConstraints, binding selectors, and param selection filter work before CEL evaluation.'],
            ['3', 'Deferred conversion', 'The dispatcher delays VersionedAttributes conversion until a policy and binding actually match.'],
            ['4', 'Decision handling', 'A false validation can deny, warn, or audit depending on the binding validationActions.'],
          ].map(([n, title, detail]) => (
            <Card key={n}>
              <CardContent className="flex gap-3 p-4">
                <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded bg-cyan-400/10 font-mono text-sm text-aqua">{n}</div>
                <div>
                  <div className="font-semibold text-white">{title}</div>
                  <div className="mt-1 text-sm leading-5 text-slate-400">{detail}</div>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      </div>
    </Section>
  );
}

function ArchitectureSection() {
  return (
    <Section
      id="architecture"
      eyebrow="Kubernetes objects"
      title="ValidatingAdmissionPolicy Architecture"
      description="VAP separates reusable policy source from bindings. The binding decides where the policy applies, what params it sees, and which actions happen when validation fails."
      icon={ShieldCheck}
    >
      <div className="grid gap-4 lg:grid-cols-[0.9fr_1.1fr]">
        <CodeBlock code={vapYaml} title="Policy and Binding YAML" />
        <div className="grid gap-3 sm:grid-cols-2">
          {[
            ['Policy', 'Holds failurePolicy, matchConstraints, variables, validations, audit annotations, and message expressions.', 'cyan'],
            ['Binding', 'Names the policy, selects resources/params, and sets Deny, Warn, or Audit actions.', 'green'],
            ['matchConstraints', 'Pre-CEL resource matching. This avoids evaluating expressions for irrelevant requests.', 'amber'],
            ['params', 'Optional parameter objects exposed as params. Missing params can skip or deny depending on binding behavior.', 'rose'],
            ['Compiled evaluator', 'Kubernetes compiles policy expressions into a Validator and reuses it by resourceVersion.', 'cyan'],
            ['PolicyHook', 'The dispatcher reads an atomic list of hooks containing policy, bindings, params, and evaluator.', 'green'],
          ].map(([title, text, tone]) => (
            <Card key={title}>
              <CardHeader>
                <CardTitle className="flex items-center justify-between gap-2">
                  {title}
                  <Badge tone={tone as 'cyan' | 'green' | 'amber' | 'rose'}>{tone}</Badge>
                </CardTitle>
              </CardHeader>
              <CardContent className="text-sm leading-6 text-slate-400">{text}</CardContent>
            </Card>
          ))}
        </div>
      </div>
    </Section>
  );
}

function CompileSection({
  expression,
  setExpression,
  evaluation,
  error,
}: {
  expression: string;
  setExpression: (value: string) => void;
  evaluation: EvaluationOutput | null;
  error: string | null;
}) {
  return (
    <Section
      id="compile"
      eyebrow="cel-go compilation"
      title="Expression String to Executable Program"
      description="The source string is parsed into an AST, checked against Kubernetes declarations, lowered into cel-go interpretable nodes, and stored as a reusable program."
      icon={Code2}
    >
      <div className="grid gap-4 xl:grid-cols-[420px_1fr]">
        <Card className="overflow-hidden">
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <FileCode2 className="h-4 w-4 text-aqua" />
              CEL editor
            </CardTitle>
            <CardDescription>Try the sample expression or string receiver calls like <code>.startsWith("te")</code>.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="overflow-hidden rounded border border-line">
              <Editor
                height="150px"
                language="cel"
                beforeMount={configureCelLanguage}
                theme="vs-dark"
                value={expression}
                onChange={(value) => setExpression(value ?? '')}
                options={{
                  minimap: { enabled: false },
                  lineNumbers: 'off',
                  wordWrap: 'on',
                  fontSize: 14,
                  scrollBeyondLastLine: false,
                  padding: { top: 12, bottom: 12 },
                }}
              />
            </div>
            {error ? (
              <div className="rounded border border-rose-400/40 bg-rose-400/10 p-3 text-sm text-rose-200">{error}</div>
            ) : (
              <TokenStrip evaluation={evaluation} />
            )}
          </CardContent>
        </Card>
        <div className="grid gap-4">
          <Card>
            <CardHeader>
              <CardTitle>Compilation stages</CardTitle>
              <CardDescription>The green badges indicate structures Kubernetes reuses after compilation.</CardDescription>
            </CardHeader>
            <CardContent>
              <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
                {compileStages.map((stage, index) => (
                  <motion.div
                    key={stage.name}
                    initial={{ opacity: 0, y: 10 }}
                    whileInView={{ opacity: 1, y: 0 }}
                    viewport={{ once: true }}
                    transition={{ delay: index * 0.04 }}
                    className="rounded-lg border border-line bg-panel2 p-3"
                  >
                    <div className="flex items-center justify-between gap-2">
                      <div className="font-semibold text-white">{stage.name}</div>
                      <Badge tone={stage.persisted ? 'green' : 'amber'}>{stage.persisted ? 'reused' : 'transient'}</Badge>
                    </div>
                    <div className="mt-2 font-mono text-xs text-aqua">{stage.artifact}</div>
                    <p className="mt-2 text-sm leading-5 text-slate-400">{stage.detail}</p>
                    <div className="mt-3 text-xs text-slate-500">{stage.cost}</div>
                  </motion.div>
                ))}
              </div>
            </CardContent>
          </Card>
          <div className="grid gap-4 lg:grid-cols-[0.9fr_1.1fr]">
            <MermaidChart chart={mermaidCompile} />
            <AstExplorer evaluation={evaluation} />
          </div>
        </div>
      </div>
    </Section>
  );
}

function TokenStrip({ evaluation }: { evaluation: EvaluationOutput | null }) {
  if (!evaluation) return null;
  return (
    <div className="flex flex-wrap gap-2">
      {evaluation.tokens
        .filter((token) => token.type !== 'eof')
        .map((token, index) => (
          <span key={`${token.start}-${index}`} className="rounded border border-line bg-slate-900 px-2 py-1 font-mono text-xs">
            <span className="text-slate-500">{token.type}</span>
            <span className="ml-1 text-slate-200">{token.value}</span>
          </span>
        ))}
    </div>
  );
}

function AstExplorer({ evaluation }: { evaluation: EvaluationOutput | null }) {
  if (!evaluation) {
    return (
      <Card>
        <CardContent className="p-4 text-sm text-slate-400">Fix the expression to see AST nodes.</CardContent>
      </Card>
    );
  }
  const rows = flattenAst(evaluation.ast);
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <GitBranch className="h-4 w-4 text-aqua" />
          AST and planned nodes
        </CardTitle>
        <CardDescription>Each AST call becomes a specialized or dispatcher-backed interpretable.</CardDescription>
      </CardHeader>
      <CardContent className="max-h-[330px] space-y-2 overflow-auto">
        {rows.map(({ node, depth }) => (
          <div
            key={node.id}
            className="rounded border border-line bg-slate-950/60 p-2"
            style={{ marginLeft: depth * 18 }}
          >
            <div className="flex flex-wrap items-center gap-2">
              <Badge tone={nodeKindTone(node)}>{node.kind}</Badge>
              <span className="font-mono text-xs text-slate-500">{node.id}</span>
            </div>
            <div className="mt-1 text-sm text-slate-200">{describeNode(node)}</div>
          </div>
        ))}
      </CardContent>
    </Card>
  );
}

function MemorySection({ evaluation }: { evaluation: EvaluationOutput | null }) {
  const [filter, setFilter] = useState('all');
  const filtered = memoryObjects.filter((item) => filter === 'all' || item.phase === filter);
  return (
    <Section
      id="memory"
      eyebrow="memory representation"
      title="What Lives Across Requests and What Gets Rebuilt"
      description="The hot path is split sharply: Kubernetes spends compile-time memory on reusable programs and request-time memory on activation, object conversion, result details, and cost accounting."
      icon={MemoryStick}
    >
      <div className="grid gap-4 xl:grid-cols-[1.1fr_0.9fr]">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Boxes className="h-4 w-4 text-aqua" />
              Object lifetime explorer
            </CardTitle>
            <CardDescription>Filter by lifetime to see allocation pressure boundaries.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <Tabs
              value={filter}
              onValueChange={setFilter}
              tabs={[
                { value: 'all', label: 'All' },
                { value: 'persistent', label: 'Persistent' },
                { value: 'compile', label: 'Compile' },
                { value: 'request', label: 'Request' },
                { value: 'pooled', label: 'Pooled' },
              ]}
            />
            <div className="grid gap-3 md:grid-cols-2">
              {filtered.map((item) => (
                <div key={item.name} className="rounded-lg border border-line bg-panel2 p-3">
                  <div className="flex items-center justify-between gap-2">
                    <div className="font-semibold text-white">{item.name}</div>
                    <Badge tone={phaseTone(item.phase)}>{item.phase}</Badge>
                  </div>
                  <div className="mt-1 text-xs text-slate-500">{item.owner}</div>
                  <dl className="mt-3 space-y-2 text-sm">
                    <MemoryPair label="stores" value={item.stores} />
                    <MemoryPair label="allocation" value={item.allocation} />
                    <MemoryPair label="reuse" value={item.reuse} />
                  </dl>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
        <div className="grid gap-4">
          <MermaidChart chart={mermaidMemory} />
          <AllocationTable rows={evaluation?.allocations ?? []} />
        </div>
      </div>
    </Section>
  );
}

function MemoryPair({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="font-mono text-xs uppercase text-slate-500">{label}</dt>
      <dd className="mt-0.5 leading-5 text-slate-300">{value}</dd>
    </div>
  );
}

function AllocationTable({ rows }: { rows: AllocationRow[] }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Allocation pressure map</CardTitle>
        <CardDescription>Visual estimates are intentionally relative; the source references show where real costs are controlled.</CardDescription>
      </CardHeader>
      <CardContent className="max-h-[340px] overflow-auto">
        <table className="w-full text-left text-sm">
          <thead className="text-xs uppercase text-slate-500">
            <tr>
              <th className="py-2 pr-3">Object</th>
              <th className="py-2 pr-3">Phase</th>
              <th className="py-2 pr-3">Estimate</th>
              <th className="py-2">Note</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line">
            {rows.map((row) => (
              <tr key={row.name}>
                <td className="py-3 pr-3 font-medium text-white">{row.name}</td>
                <td className="py-3 pr-3">
                  <Badge tone={phaseTone(row.phase)}>{row.phase}</Badge>
                </td>
                <td className="py-3 pr-3 font-mono text-xs text-amber-200">{row.estimate}</td>
                <td className="py-3 leading-5 text-slate-400">{row.note}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </CardContent>
    </Card>
  );
}

function RuntimeSection({ evaluation }: { evaluation: EvaluationOutput | null }) {
  const [selected, setSelected] = useState('env');
  const panels = {
    env: {
      title: 'cel.Env',
      icon: Database,
      text: 'Kubernetes builds EnvSet templates with declarations for object, oldObject, request, params, namespaceObject, authorizer, libraries, and cost options. cel-go Env owns parser/checker setup and a shared dispatcher cache.',
      bullets: ['env.Compile parses and type-checks', 'env.Program plans an executable', 'shared dispatcher reduces retained memory'],
    },
    program: {
      title: 'cel.Program',
      icon: CircuitBoard,
      text: 'A Program stores the interpreter, dispatcher, attribute factory, cost estimator, and root Interpretable. Kubernetes keeps it inside compiled validation conditions.',
      bullets: ['Program.ContextEval wraps activation', 'interpreter root Eval returns a ref.Val', 'cost details flow back to Kubernetes'],
    },
    tree: {
      title: 'Interpretable tree',
      icon: Split,
      text: 'The planner lowers checked AST calls to evalAnd, evalEq, evalAttr, literals, and generic function nodes. The tree is immutable and reused.',
      bullets: ['evalAnd short-circuits', 'evalEq calls types.Equal', 'evalAttr resolves absolute attributes'],
    },
  } as const;
  const current = panels[selected as keyof typeof panels];
  const Icon = current.icon;

  return (
    <Section
      id="runtime"
      eyebrow="cel-go runtime"
      title="Program Execution Internals"
      description="Request evaluation does not reparse or recheck the expression. It hands a fresh Activation to a persistent cel.Program and walks the planned tree."
      icon={Zap}
    >
      <div className="grid gap-4 lg:grid-cols-[360px_1fr]">
        <Card>
          <CardHeader>
            <CardTitle>Runtime layers</CardTitle>
            <CardDescription>Select a layer to inspect its responsibilities.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-2">
            {Object.entries(panels).map(([key, value]) => {
              const LayerIcon = value.icon;
              return (
                <button
                  key={key}
                  type="button"
                  onClick={() => setSelected(key)}
                  className={cn(
                    'flex w-full items-center gap-3 rounded-md border border-line bg-panel2 p-3 text-left transition-colors hover:bg-slate-800',
                    selected === key && 'border-aqua bg-cyan-400/10',
                  )}
                >
                  <LayerIcon className="h-5 w-5 text-aqua" />
                  <span className="font-medium text-white">{value.title}</span>
                </button>
              );
            })}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Icon className="h-5 w-5 text-aqua" />
              {current.title}
            </CardTitle>
            <CardDescription>{current.text}</CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4 lg:grid-cols-[1fr_1fr]">
            <div className="space-y-3">
              {current.bullets.map((bullet) => (
                <div key={bullet} className="flex items-start gap-3 rounded border border-line bg-panel2 p-3 text-sm text-slate-300">
                  <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0 text-mint" />
                  {bullet}
                </div>
              ))}
            </div>
            <div className="rounded-lg border border-line bg-slate-950 p-3">
              <div className="mb-2 flex items-center gap-2 text-sm font-semibold text-white">
                <Activity className="h-4 w-4 text-amber" />
                Current evaluation trace
              </div>
              <div className="max-h-[260px] space-y-2 overflow-auto">
                {(evaluation?.steps ?? []).slice(0, 8).map((step) => (
                  <TraceRow key={step.id} step={step} compact />
                ))}
              </div>
            </div>
          </CardContent>
        </Card>
      </div>
    </Section>
  );
}

function DispatcherSection() {
  return (
    <Section
      id="dispatch"
      eyebrow="overloads and traits"
      title="Dispatcher and Overload Resolution"
      description="The checker records overload ids on calls. The planner uses those ids to choose special nodes or dispatcher-backed function calls with trait checks."
      icon={Binary}
    >
      <Card>
        <CardContent className="overflow-auto p-0">
          <table className="w-full min-w-[900px] text-left text-sm">
            <thead className="bg-panel2 text-xs uppercase text-slate-500">
              <tr>
                <th className="px-4 py-3">CEL syntax</th>
                <th className="px-4 py-3">Overload / shape</th>
                <th className="px-4 py-3">cel-go path</th>
                <th className="px-4 py-3">Dispatch behavior</th>
                <th className="px-4 py-3">Traits</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              {dispatcherRows.map((row) => (
                <tr key={row.syntax} className="align-top">
                  <td className="px-4 py-4 font-mono text-cyan-100">{row.syntax}</td>
                  <td className="px-4 py-4 font-mono text-xs text-amber-200">{row.overload}</td>
                  <td className="px-4 py-4 text-slate-300">{row.celGoPath}</td>
                  <td className="px-4 py-4 text-slate-400">{row.dispatch}</td>
                  <td className="px-4 py-4 text-slate-400">{row.trait}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </CardContent>
      </Card>
      <div className="grid gap-4 md:grid-cols-3">
        <InfoCard icon={Search} title="Checker phase" text="Function candidates are filtered by call style and assignability. The surviving overload ids are recorded in the AST reference map." />
        <InfoCard icon={TableProperties} title="Dispatcher table" text="defaultDispatcher indexes overload implementations by operator/function id, with a parent dispatcher for shared env bindings." />
        <InfoCard icon={Layers3} title="Runtime traits" text="Generic call nodes check traits like ReceiverType, MapperType, IndexerType, and ContainerType before invoking overload code." />
      </div>
    </Section>
  );
}

function ActivationSection() {
  return (
    <Section
      id="activation"
      eyebrow="request context"
      title="Activation and Attribute Resolution"
      description="Kubernetes injects request-local values into a cel-go Activation. Attribute resolution starts by resolving a variable name, then applies a qualifier chain."
      icon={Database}
    >
      <div className="grid gap-4 lg:grid-cols-[0.9fr_1.1fr]">
        <Card>
          <CardHeader>
            <CardTitle>Injected variables</CardTitle>
            <CardDescription>These are created for runtime evaluation, not during compilation.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            {variableRows.map((row) => (
              <div key={row.name} className="rounded border border-line bg-panel2 p-3">
                <div className="flex items-center justify-between gap-2">
                  <span className="font-mono text-sm text-aqua">{row.name}</span>
                  <Badge tone="amber">{row.lifetime}</Badge>
                </div>
                <div className="mt-2 text-sm text-slate-300">{row.source}</div>
                <div className="mt-1 text-xs leading-5 text-slate-500">{row.note}</div>
              </div>
            ))}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Eye className="h-4 w-4 text-aqua" />
              Resolving object.metadata.labels.environment
            </CardTitle>
            <CardDescription>cel-go's absolute attribute starts at object and applies each qualifier.</CardDescription>
          </CardHeader>
          <CardContent>
            <div className="space-y-3">
              {[
                ['ResolveName("object")', 'evaluationActivation returns the converted admission object value.', 'activation'],
                ['qualifier: metadata', 'Attribute factory applies a field/map qualifier to the object.', 'attribute'],
                ['qualifier: labels', 'The nested map is adapted to a CEL value with mapper/indexer traits.', 'attribute'],
                ['qualifier: environment', 'The map key lookup returns the string label value.', 'attribute'],
                ['literal: "prod"', 'The right-hand literal is already embedded in the program.', 'literal'],
                ['evalEq', 'types.Equal compares the resolved ref.Val values.', 'dispatch'],
              ].map(([title, text, phase], index) => (
                <motion.div
                  key={title}
                  initial={{ opacity: 0, x: -12 }}
                  whileInView={{ opacity: 1, x: 0 }}
                  viewport={{ once: true }}
                  transition={{ delay: index * 0.06 }}
                  className="flex items-start gap-3"
                >
                  <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded bg-cyan-400/10 font-mono text-xs text-aqua">
                    {index + 1}
                  </div>
                  <div className="rounded border border-line bg-panel2 p-3">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-sm text-white">{title}</span>
                      <Badge tone={phase === 'dispatch' ? 'rose' : phase === 'literal' ? 'slate' : 'cyan'}>{phase}</Badge>
                    </div>
                    <div className="mt-1 text-sm leading-5 text-slate-400">{text}</div>
                  </div>
                </motion.div>
              ))}
            </div>
          </CardContent>
        </Card>
      </div>
    </Section>
  );
}

function SimulatorSection({
  expression,
  setExpression,
  environment,
  setEnvironment,
  name,
  setName,
  evaluation,
  error,
}: {
  expression: string;
  setExpression: (value: string) => void;
  environment: string;
  setEnvironment: (value: string) => void;
  name: string;
  setName: (value: string) => void;
  evaluation: EvaluationOutput | null;
  error: string | null;
}) {
  const [activeStep, setActiveStep] = useState(0);
  const [playing, setPlaying] = useState(false);

  useEffect(() => {
    setActiveStep(0);
    setPlaying(false);
  }, [expression, environment, name]);

  useEffect(() => {
    if (!playing || !evaluation) return;
    if (activeStep >= evaluation.steps.length - 1) {
      setPlaying(false);
      return;
    }
    const timer = window.setTimeout(() => setActiveStep((step) => step + 1), 850);
    return () => window.clearTimeout(timer);
  }, [activeStep, evaluation, playing]);

  const current = evaluation?.steps[activeStep];

  return (
    <Section
      id="simulator"
      eyebrow="interactive evaluator"
      title="Runtime Evaluation Simulator"
      description="Modify the request object and replay the AST walk. When the first equality is false, the right-hand name check is skipped by evalAnd."
      icon={Bug}
    >
      <div className="grid gap-4 xl:grid-cols-[410px_1fr]">
        <Card>
          <CardHeader>
            <CardTitle>Request controls</CardTitle>
            <CardDescription>These values become fields under the per-request object activation variable.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <label className="block space-y-2 text-sm">
              <span className="text-slate-300">metadata.labels.environment</span>
              <input
                value={environment}
                onChange={(event) => setEnvironment(event.target.value)}
                className="w-full rounded-md border border-line bg-slate-950 px-3 py-2 font-mono text-sm text-white outline-none focus:border-aqua"
              />
            </label>
            <label className="block space-y-2 text-sm">
              <span className="text-slate-300">metadata.name</span>
              <input
                value={name}
                onChange={(event) => setName(event.target.value)}
                className="w-full rounded-md border border-line bg-slate-950 px-3 py-2 font-mono text-sm text-white outline-none focus:border-aqua"
              />
            </label>
            <div className="flex flex-wrap gap-2">
              <Button onClick={() => setPlaying((value) => !value)} disabled={!evaluation}>
                <Play className="h-4 w-4" />
                {playing ? 'Pause' : 'Run'}
              </Button>
              <Button variant="secondary" onClick={() => setActiveStep(0)} disabled={!evaluation}>
                <TimerReset className="h-4 w-4" />
                Reset
              </Button>
              <Button
                variant="secondary"
                onClick={() => {
                  setEnvironment('prod');
                  setName('test');
                  setExpression(sampleExpression);
                }}
              >
                <RefreshCw className="h-4 w-4" />
                Sample
              </Button>
            </div>
            {error ? (
              <div className="rounded border border-rose-400/40 bg-rose-400/10 p-3 text-sm text-rose-200">{error}</div>
            ) : (
              <div className={cn('rounded-lg border p-4', evaluation?.allowed ? 'border-green-400/40 bg-green-400/10' : 'border-rose-400/40 bg-rose-400/10')}>
                <div className="flex items-center gap-2 text-sm font-semibold">
                  {evaluation?.allowed ? <CheckCircle2 className="h-5 w-5 text-mint" /> : <XCircle className="h-5 w-5 text-rose" />}
                  <span className={evaluation?.allowed ? 'text-green-200' : 'text-rose-200'}>
                    {evaluation?.allowed ? 'Admission would allow' : 'Admission would deny'}
                  </span>
                </div>
                <div className="mt-2 text-sm leading-5 text-slate-300">{evaluation?.estimates.relativeCost}</div>
              </div>
            )}
          </CardContent>
        </Card>
        <div className="grid gap-4 lg:grid-cols-[1fr_380px]">
          <Card>
            <CardHeader>
              <CardTitle>Animated trace</CardTitle>
              <CardDescription>Active step {evaluation ? activeStep + 1 : 0} of {evaluation?.steps.length ?? 0}</CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              <div className="h-2 overflow-hidden rounded bg-slate-800">
                <motion.div
                  className="h-full bg-aqua"
                  animate={{ width: evaluation ? `${((activeStep + 1) / evaluation.steps.length) * 100}%` : '0%' }}
                />
              </div>
              <div className="max-h-[460px] space-y-2 overflow-auto pr-1">
                <AnimatePresence initial={false}>
                  {(evaluation?.steps ?? []).map((step, index) => (
                    <motion.button
                      key={step.id}
                      type="button"
                      onClick={() => setActiveStep(index)}
                      initial={{ opacity: 0, y: 8 }}
                      animate={{ opacity: 1, y: 0 }}
                      exit={{ opacity: 0 }}
                      className={cn(
                        'w-full rounded-md border p-3 text-left transition-colors',
                        index === activeStep ? 'border-aqua bg-cyan-400/10' : 'border-line bg-panel2 hover:bg-slate-800',
                      )}
                    >
                      <TraceRow step={step} />
                    </motion.button>
                  ))}
                </AnimatePresence>
              </div>
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Current frame</CardTitle>
              <CardDescription>Node, value, reuse, and allocation signal.</CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              {current ? (
                <>
                  <Badge tone={phaseTone(current.phase)}>{current.phase}</Badge>
                  <div className="font-mono text-sm text-white">{current.label}</div>
                  <p className="text-sm leading-6 text-slate-400">{current.detail}</p>
                  <div className="rounded border border-line bg-slate-950 p-3">
                    <div className="text-xs uppercase text-slate-500">value</div>
                    <pre className="mt-2 overflow-auto font-mono text-xs text-cyan-100">{JSON.stringify(current.value, null, 2)}</pre>
                  </div>
                  <div className="grid grid-cols-2 gap-2">
                    <Metric icon={Clock3} label="reuse" value={current.reused ? 'yes' : 'no'} tone={current.reused ? 'green' : 'amber'} />
                    <Metric icon={MemoryStick} label="alloc" value={`${current.allocationBytes} B`} tone={current.allocationBytes ? 'amber' : 'green'} />
                  </div>
                </>
              ) : (
                <div className="text-sm text-slate-400">No frame selected.</div>
              )}
            </CardContent>
          </Card>
        </div>
      </div>
    </Section>
  );
}

function SourceSection() {
  const [query, setQuery] = useState('');
  const rows = sourceMap.filter((row) => `${row.concept} ${row.file} ${row.detail}`.toLowerCase().includes(query.toLowerCase()));
  return (
    <Section
      id="source"
      eyebrow="source code explorer"
      title="Where the Concepts Live in Kubernetes and cel-go"
      description="These source landmarks map the visual model to actual implementation files in this repository and the vendored cel-go tree."
      icon={FileCode2}
    >
      <Card>
        <CardHeader>
          <CardTitle>Source map</CardTitle>
          <CardDescription>Search by concept, file, or subsystem.</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="relative">
            <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-500" />
            <input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="Search dispatcher, activation, checker, cache..."
              className="w-full rounded-md border border-line bg-slate-950 py-2 pl-10 pr-3 text-sm text-white outline-none focus:border-aqua"
            />
          </div>
          <div className="grid gap-3 md:grid-cols-2">
            {rows.map((row) => (
              <div key={`${row.concept}-${row.file}`} className="rounded-lg border border-line bg-panel2 p-3">
                <div className="flex items-start justify-between gap-3">
                  <div className="font-semibold text-white">{row.concept}</div>
                  <Badge tone={row.file.startsWith('vendor') ? 'amber' : 'cyan'}>{row.file.startsWith('vendor') ? 'cel-go' : 'k8s'}</Badge>
                </div>
                <div className="mt-2 break-all font-mono text-xs text-aqua">{row.file}</div>
                <div className="mt-2 text-sm leading-5 text-slate-400">{row.detail}</div>
              </div>
            ))}
          </div>
        </CardContent>
      </Card>
    </Section>
  );
}

function CodeBlock({ code, title }: { code: string; title: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <FileCode2 className="h-4 w-4 text-aqua" />
          {title}
        </CardTitle>
      </CardHeader>
      <CardContent>
        <pre className="max-h-[520px] overflow-auto rounded border border-line bg-slate-950 p-4 font-mono text-xs leading-5 text-slate-200">
          {code}
        </pre>
      </CardContent>
    </Card>
  );
}

function InfoCard({ icon: Icon, title, text }: { icon: typeof Search; title: string; text: string }) {
  return (
    <Card>
      <CardContent className="p-4">
        <div className="flex items-center gap-2 font-semibold text-white">
          <Icon className="h-4 w-4 text-aqua" />
          {title}
        </div>
        <p className="mt-2 text-sm leading-6 text-slate-400">{text}</p>
      </CardContent>
    </Card>
  );
}

function TraceRow({ step, compact = false }: { step: EvaluationStep; compact?: boolean }) {
  return (
    <div className={cn('flex gap-3', compact && 'text-xs')}>
      <Badge tone={phaseTone(step.phase)} className="h-fit shrink-0">
        {step.phase}
      </Badge>
      <div className="min-w-0">
        <div className="truncate font-mono text-sm text-white">{step.label}</div>
        {!compact && <div className="mt-1 text-sm leading-5 text-slate-400">{step.detail}</div>}
        <div className="mt-1 flex flex-wrap gap-2 text-xs text-slate-500">
          <span>node {step.nodeId}</span>
          <span>{step.reused ? 'reused' : 'request-local'}</span>
          <span>{step.allocationBytes} B</span>
        </div>
      </div>
    </div>
  );
}

function MermaidChart({ chart }: { chart: string }) {
  const ref = useRef<HTMLDivElement>(null);
  const id = useMemo(() => `mermaid-${Math.random().toString(36).slice(2)}`, []);

  useEffect(() => {
    let cancelled = false;
    mermaid.initialize({
      startOnLoad: false,
      theme: 'dark',
      securityLevel: 'loose',
      themeVariables: {
        background: '#0d121a',
        primaryColor: '#121925',
        primaryBorderColor: '#273243',
        primaryTextColor: '#e2e8f0',
        lineColor: '#22d3ee',
        secondaryColor: '#0f172a',
        tertiaryColor: '#111827',
      },
    });
    mermaid.render(id, chart).then(({ svg }) => {
      if (!cancelled && ref.current) ref.current.innerHTML = svg;
    });
    return () => {
      cancelled = true;
    };
  }, [chart, id]);

  return (
    <Card>
      <CardContent className="mermaid-box p-4">
        <div ref={ref} />
      </CardContent>
    </Card>
  );
}

function configureCelLanguage(monaco: Monaco) {
  if (!monaco.languages.getLanguages().some((language) => language.id === 'cel')) {
    monaco.languages.register({ id: 'cel' });
    monaco.languages.setMonarchTokensProvider('cel', {
      tokenizer: {
        root: [
          [/"([^"\\]|\\.)*$/, 'string.invalid'],
          [/"/, 'string', '@string'],
          [/\b(object|oldObject|request|params|namespaceObject|authorizer|variables|null|true|false)\b/, 'keyword'],
          [/[a-zA-Z_][\w]*/, 'identifier'],
          [/==|!=|&&|\|\||<=|>=|in\b/, 'operator'],
          [/[{}()[\].,]/, 'delimiter'],
        ],
        string: [
          [/[^\\"]+/, 'string'],
          [/\\./, 'string.escape'],
          [/"/, 'string', '@pop'],
        ],
      },
    });
  }
}

function phaseTone(phase: string): 'cyan' | 'green' | 'amber' | 'rose' | 'slate' {
  if (phase === 'persistent' || phase === 'pooled' || phase === 'decision') return 'green';
  if (phase === 'request' || phase === 'activation' || phase === 'compile') return 'amber';
  if (phase === 'temporary' || phase === 'dispatch') return 'rose';
  if (phase === 'attribute') return 'cyan';
  return 'slate';
}

export default App;
