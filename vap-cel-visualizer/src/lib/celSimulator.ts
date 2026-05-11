export const sampleExpression =
  'object.metadata.labels.environment == "prod" && object.metadata.name == "test"';

export type TokenType =
  | 'identifier'
  | 'string'
  | 'dot'
  | 'eq'
  | 'and'
  | 'lparen'
  | 'rparen'
  | 'comma'
  | 'lbracket'
  | 'rbracket'
  | 'eof';

export type Token = {
  type: TokenType;
  value: string;
  start: number;
  end: number;
};

export type CelNode =
  | {
      id: string;
      kind: 'path';
      root: string;
      qualifiers: string[];
    }
  | {
      id: string;
      kind: 'literal';
      value: string | boolean | null;
      valueType: 'string' | 'bool' | 'null';
    }
  | {
      id: string;
      kind: 'eq';
      overloadId: '_==_';
      left: CelNode;
      right: CelNode;
    }
  | {
      id: string;
      kind: 'and';
      overloadId: '_&&_';
      left: CelNode;
      right: CelNode;
    }
  | {
      id: string;
      kind: 'call';
      functionName: string;
      overloadId: string;
      receiver: CelNode | null;
      args: CelNode[];
    };

export type EvaluationStep = {
  id: string;
  nodeId: string;
  phase: 'activation' | 'attribute' | 'literal' | 'dispatch' | 'interpreter' | 'decision';
  label: string;
  detail: string;
  value: unknown;
  reused: boolean;
  allocationBytes: number;
};

export type AllocationRow = {
  name: string;
  phase: 'compile' | 'persistent' | 'request' | 'temporary' | 'pooled';
  lifetime: string;
  estimate: string;
  note: string;
};

export type EvaluationOutput = {
  ast: CelNode;
  tokens: Token[];
  result: boolean | null;
  allowed: boolean;
  steps: EvaluationStep[];
  skippedNodeIds: string[];
  estimates: {
    steps: number;
    requestAllocBytes: number;
    transientAllocBytes: number;
    relativeCost: string;
  };
  allocations: AllocationRow[];
};

let nodeCounter = 0;
let stepCounter = 0;

function nextNodeId(prefix: string) {
  nodeCounter += 1;
  return `${prefix}-${nodeCounter}`;
}

function nextStepId() {
  stepCounter += 1;
  return `step-${stepCounter}`;
}

export function tokenize(input: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < input.length) {
    const ch = input[i];
    if (/\s/.test(ch)) {
      i += 1;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      const start = i;
      i += 1;
      while (i < input.length && /[A-Za-z0-9_]/.test(input[i])) i += 1;
      tokens.push({ type: 'identifier', value: input.slice(start, i), start, end: i });
      continue;
    }
    if (ch === '"') {
      const start = i;
      i += 1;
      let value = '';
      while (i < input.length && input[i] !== '"') {
        if (input[i] === '\\' && i + 1 < input.length) {
          value += input[i + 1];
          i += 2;
        } else {
          value += input[i];
          i += 1;
        }
      }
      if (input[i] !== '"') {
        throw new Error(`Unterminated string literal at ${start}`);
      }
      i += 1;
      tokens.push({ type: 'string', value, start, end: i });
      continue;
    }
    if (input.startsWith('==', i)) {
      tokens.push({ type: 'eq', value: '==', start: i, end: i + 2 });
      i += 2;
      continue;
    }
    if (input.startsWith('&&', i)) {
      tokens.push({ type: 'and', value: '&&', start: i, end: i + 2 });
      i += 2;
      continue;
    }
    const simple: Record<string, TokenType> = {
      '.': 'dot',
      '(': 'lparen',
      ')': 'rparen',
      ',': 'comma',
      '[': 'lbracket',
      ']': 'rbracket',
    };
    if (simple[ch]) {
      tokens.push({ type: simple[ch], value: ch, start: i, end: i + 1 });
      i += 1;
      continue;
    }
    throw new Error(`Unexpected character "${ch}" at ${i}`);
  }
  tokens.push({ type: 'eof', value: '', start: input.length, end: input.length });
  return tokens;
}

class Parser {
  private pos = 0;

  constructor(private readonly tokens: Token[]) {}

  parseExpression(): CelNode {
    const expr = this.parseAnd();
    this.expect('eof');
    return expr;
  }

  private parseAnd(): CelNode {
    let left = this.parseEquality();
    while (this.match('and')) {
      const right = this.parseEquality();
      left = { id: nextNodeId('and'), kind: 'and', overloadId: '_&&_', left, right };
    }
    return left;
  }

  private parseEquality(): CelNode {
    let left = this.parsePostfix();
    while (this.match('eq')) {
      const right = this.parsePostfix();
      left = { id: nextNodeId('eq'), kind: 'eq', overloadId: '_==_', left, right };
    }
    return left;
  }

  private parsePostfix(): CelNode {
    let node = this.parsePrimary();
    while (true) {
      if (this.match('dot')) {
        const ident = this.expect('identifier').value;
        if (this.match('lparen')) {
          const args = this.parseArgs();
          node = {
            id: nextNodeId('call'),
            kind: 'call',
            functionName: ident,
            overloadId: `${ident}_string`,
            receiver: node,
            args,
          };
        } else if (node.kind === 'path') {
          node = { ...node, qualifiers: [...node.qualifiers, ident] };
        } else {
          node = {
            id: nextNodeId('select'),
            kind: 'call',
            functionName: 'select',
            overloadId: 'select',
            receiver: node,
            args: [{ id: nextNodeId('lit'), kind: 'literal', value: ident, valueType: 'string' }],
          };
        }
        continue;
      }
      if (this.match('lbracket')) {
        const key = this.expect('string').value;
        this.expect('rbracket');
        if (node.kind === 'path') {
          node = { ...node, qualifiers: [...node.qualifiers, key] };
        } else {
          node = {
            id: nextNodeId('index'),
            kind: 'call',
            functionName: '_[_]',
            overloadId: 'index',
            receiver: node,
            args: [{ id: nextNodeId('lit'), kind: 'literal', value: key, valueType: 'string' }],
          };
        }
        continue;
      }
      return node;
    }
  }

  private parsePrimary(): CelNode {
    if (this.peek().type === 'identifier') {
      const ident = this.expect('identifier').value;
      if (ident === 'true' || ident === 'false') {
        return { id: nextNodeId('lit'), kind: 'literal', value: ident === 'true', valueType: 'bool' };
      }
      if (ident === 'null') {
        return { id: nextNodeId('lit'), kind: 'literal', value: null, valueType: 'null' };
      }
      return { id: nextNodeId('path'), kind: 'path', root: ident, qualifiers: [] };
    }
    if (this.peek().type === 'string') {
      return { id: nextNodeId('lit'), kind: 'literal', value: this.expect('string').value, valueType: 'string' };
    }
    if (this.match('lparen')) {
      const expr = this.parseAnd();
      this.expect('rparen');
      return expr;
    }
    throw new Error(`Expected expression at token ${this.peek().start}`);
  }

  private parseArgs(): CelNode[] {
    const args: CelNode[] = [];
    if (this.match('rparen')) return args;
    do {
      args.push(this.parseAnd());
    } while (this.match('comma'));
    this.expect('rparen');
    return args;
  }

  private peek() {
    return this.tokens[this.pos];
  }

  private match(type: TokenType) {
    if (this.peek().type !== type) return false;
    this.pos += 1;
    return true;
  }

  private expect(type: TokenType) {
    const token = this.peek();
    if (token.type !== type) {
      throw new Error(`Expected ${type}, got ${token.type} at ${token.start}`);
    }
    this.pos += 1;
    return token;
  }
}

export function parseCel(input: string): { tokens: Token[]; ast: CelNode } {
  nodeCounter = 0;
  const tokens = tokenize(input);
  const ast = new Parser(tokens).parseExpression();
  return { tokens, ast };
}

type Activation = {
  object: {
    metadata: {
      labels: Record<string, string>;
      name: string;
    };
  };
  oldObject: unknown;
  params: unknown;
  request: {
    operation: string;
    resource: { group: string; version: string; resource: string };
    namespace?: string;
  };
};

function formatValue(value: unknown) {
  if (typeof value === 'string') return `"${value}"`;
  if (value === undefined) return 'undefined';
  return JSON.stringify(value);
}

function addStep(
  steps: EvaluationStep[],
  step: Omit<EvaluationStep, 'id'>,
) {
  steps.push({ id: nextStepId(), ...step });
}

function resolvePath(node: Extract<CelNode, { kind: 'path' }>, activation: Activation, steps: EvaluationStep[]) {
  let current: unknown = (activation as unknown as Record<string, unknown>)[node.root];
  addStep(steps, {
    nodeId: node.id,
    phase: 'activation',
    label: `ResolveName("${node.root}")`,
    detail: 'Kubernetes evaluationActivation.ResolveName switches over declared variables.',
    value: current,
    reused: false,
    allocationBytes: 0,
  });

  for (const qualifier of node.qualifiers) {
    const before = current;
    if (before && typeof before === 'object') {
      current = (before as Record<string, unknown>)[qualifier];
    } else {
      current = undefined;
    }
    addStep(steps, {
      nodeId: node.id,
      phase: 'attribute',
      label: `qualify .${qualifier}`,
      detail: 'cel-go applies an attribute qualifier; maps use mapper/indexer traits after native adaptation.',
      value: current,
      reused: true,
      allocationBytes: 16,
    });
  }
  return current;
}

function evalNode(
  node: CelNode,
  activation: Activation,
  steps: EvaluationStep[],
  skippedNodeIds: string[],
): unknown {
  if (node.kind === 'literal') {
    addStep(steps, {
      nodeId: node.id,
      phase: 'literal',
      label: `${node.valueType} literal`,
      detail: 'Literal nodes are embedded in the checked AST and planned interpretable tree.',
      value: node.value,
      reused: true,
      allocationBytes: 0,
    });
    return node.value;
  }

  if (node.kind === 'path') {
    return resolvePath(node, activation, steps);
  }

  if (node.kind === 'eq') {
    addStep(steps, {
      nodeId: node.id,
      phase: 'dispatch',
      label: 'evalEq special form',
      detail: 'The checked overload id is _==_; cel-go plans equality as evalEq and calls types.Equal.',
      value: node.overloadId,
      reused: true,
      allocationBytes: 0,
    });
    const left = evalNode(node.left, activation, steps, skippedNodeIds);
    const right = evalNode(node.right, activation, steps, skippedNodeIds);
    const value = left === right;
    addStep(steps, {
      nodeId: node.id,
      phase: 'interpreter',
      label: `${formatValue(left)} == ${formatValue(right)}`,
      detail: 'The equality node returns a CEL bool ref.Val, then Kubernetes checks it against types.True.',
      value,
      reused: false,
      allocationBytes: 24,
    });
    return value;
  }

  if (node.kind === 'and') {
    addStep(steps, {
      nodeId: node.id,
      phase: 'dispatch',
      label: 'evalAnd short-circuit node',
      detail: 'Logical AND is represented as a call in the AST, but the planner emits a short-circuit interpretable.',
      value: node.overloadId,
      reused: true,
      allocationBytes: 0,
    });
    const left = evalNode(node.left, activation, steps, skippedNodeIds);
    if (left === false) {
      collectNodeIds(node.right).forEach((id) => skippedNodeIds.push(id));
      addStep(steps, {
        nodeId: node.id,
        phase: 'interpreter',
        label: 'short-circuit false',
        detail: 'The right side is not evaluated, so no name lookup or qualifier walk happens for it.',
        value: false,
        reused: false,
        allocationBytes: 8,
      });
      return false;
    }
    const right = evalNode(node.right, activation, steps, skippedNodeIds);
    const value = Boolean(left && right);
    addStep(steps, {
      nodeId: node.id,
      phase: 'interpreter',
      label: `AND result ${value}`,
      detail: 'Both branches produced true-like CEL values, so the conjunction can return.',
      value,
      reused: false,
      allocationBytes: 8,
    });
    return value;
  }

  if (node.kind === 'call') {
    const receiver = node.receiver ? evalNode(node.receiver, activation, steps, skippedNodeIds) : undefined;
    const args = node.args.map((arg) => evalNode(arg, activation, steps, skippedNodeIds));
    addStep(steps, {
      nodeId: node.id,
      phase: 'dispatch',
      label: `FindOverload(${node.overloadId})`,
      detail: 'The dispatcher checks the local overload map first, then its parent shared dispatcher.',
      value: node.overloadId,
      reused: true,
      allocationBytes: 0,
    });
    let value: unknown;
    if (node.functionName === 'contains') {
      value = typeof receiver === 'string' && typeof args[0] === 'string' ? receiver.includes(args[0]) : false;
    } else if (node.functionName === 'startsWith') {
      value = typeof receiver === 'string' && typeof args[0] === 'string' ? receiver.startsWith(args[0]) : false;
    } else if (node.functionName === 'select' || node.functionName === '_[_]') {
      const key = args[0];
      value = receiver && typeof receiver === 'object' && typeof key === 'string'
        ? (receiver as Record<string, unknown>)[key]
        : undefined;
    } else {
      value = false;
    }
    addStep(steps, {
      nodeId: node.id,
      phase: 'interpreter',
      label: `${node.functionName}() -> ${formatValue(value)}`,
      detail: 'Generic function calls go through evalBinary/evalVarArgs with trait and receiver checks.',
      value,
      reused: false,
      allocationBytes: 32,
    });
    return value;
  }
}

export function evaluateCel(
  expression: string,
  input: {
    environment: string;
    name: string;
    operation?: string;
    resource?: string;
    namespace?: string;
  },
): EvaluationOutput {
  stepCounter = 0;
  const { tokens, ast } = parseCel(expression);
  const activation: Activation = {
    object: {
      metadata: {
        labels: { environment: input.environment },
        name: input.name,
      },
    },
    oldObject: null,
    params: null,
    request: {
      operation: input.operation ?? 'CREATE',
      resource: { group: '', version: 'v1', resource: input.resource ?? 'pods' },
      namespace: input.namespace,
    },
  };
  const steps: EvaluationStep[] = [];
  const skippedNodeIds: string[] = [];
  addStep(steps, {
    nodeId: ast.id,
    phase: 'activation',
    label: 'newActivation(...)',
    detail: 'Kubernetes builds a per-request evaluationActivation with object, oldObject, request, params, namespaceObject, and authorizer.',
    value: 'evaluationActivation',
    reused: false,
    allocationBytes: 192,
  });
  const result = evalNode(ast, activation, steps, skippedNodeIds);
  const allowed = result === true;
  addStep(steps, {
    nodeId: ast.id,
    phase: 'decision',
    label: allowed ? 'admission allowed' : 'admission denied',
    detail: allowed
      ? 'The validation result is true, so the VAP validator emits no denied validation.'
      : 'A validation expression returned false, so validationActions can deny, warn, or audit.',
    value: allowed,
    reused: false,
    allocationBytes: 24,
  });

  const requestAllocBytes = steps
    .filter((step) => !step.reused)
    .reduce((sum, step) => sum + step.allocationBytes, 0);
  const transientAllocBytes = steps.reduce((sum, step) => sum + step.allocationBytes, 0);
  return {
    ast,
    tokens,
    result: typeof result === 'boolean' ? result : null,
    allowed,
    steps,
    skippedNodeIds,
    estimates: {
      steps: steps.length,
      requestAllocBytes,
      transientAllocBytes,
      relativeCost:
        skippedNodeIds.length > 0
          ? 'low: short-circuit avoided the second equality'
          : 'normal: both equality branches and both attribute paths were walked',
    },
    allocations: buildAllocationRows(requestAllocBytes, transientAllocBytes),
  };
}

function buildAllocationRows(requestAllocBytes: number, transientAllocBytes: number): AllocationRow[] {
  return [
    {
      name: 'PolicyHook / Validator',
      phase: 'persistent',
      lifetime: 'Informer refresh until policy resourceVersion changes',
      estimate: 'small wrapper, references compiled evaluators',
      note: 'Stored in an atomic policy list and reused by the dispatcher.',
    },
    {
      name: 'cel.Program + Interpretable tree',
      phase: 'persistent',
      lifetime: 'Compile cache entry / compiled policy',
      estimate: '~150-250 KB per Kubernetes compile-cache comments',
      note: 'Contains the planned tree, dispatcher reference, attribute factory, and cost options.',
    },
    {
      name: 'cel.Env / EnvSet',
      phase: 'persistent',
      lifetime: 'Process-wide base env and per-template env variants',
      estimate: 'expensive to extend; intentionally reused',
      note: 'Kubernetes memoizes base environments and optional variable declaration envs.',
    },
    {
      name: 'evaluationActivation',
      phase: 'request',
      lifetime: 'One admission evaluation',
      estimate: `${requestAllocBytes} B in this visual estimate`,
      note: 'Carries object, oldObject, request, params, namespaceObject, and authorizer handles.',
    },
    {
      name: 'VersionedAttributes / unstructured object',
      phase: 'request',
      lifetime: 'Only if a matching policy needs conversion',
      estimate: 'can dominate request-time allocation',
      note: 'Dispatcher defers conversion until policy, binding, and params match.',
    },
    {
      name: 'ctxActivation wrapper',
      phase: 'pooled',
      lifetime: 'Borrowed around ContextEval',
      estimate: 'reused via cel-go pool',
      note: 'Adds cancellation checks around the Kubernetes activation.',
    },
    {
      name: 'EvalDetails / cost tracker values',
      phase: 'temporary',
      lifetime: 'During Eval',
      estimate: `${transientAllocBytes} B visual transient estimate`,
      note: 'Used for cost accounting and result details, then discarded.',
    },
  ];
}

export function collectNodeIds(node: CelNode): string[] {
  const ids = [node.id];
  if (node.kind === 'eq' || node.kind === 'and') {
    ids.push(...collectNodeIds(node.left), ...collectNodeIds(node.right));
  }
  if (node.kind === 'call') {
    if (node.receiver) ids.push(...collectNodeIds(node.receiver));
    node.args.forEach((arg) => ids.push(...collectNodeIds(arg)));
  }
  return ids;
}

export function flattenAst(node: CelNode, depth = 0): Array<{ node: CelNode; depth: number }> {
  const rows = [{ node, depth }];
  if (node.kind === 'and' || node.kind === 'eq') {
    rows.push(...flattenAst(node.left, depth + 1), ...flattenAst(node.right, depth + 1));
  }
  if (node.kind === 'call') {
    if (node.receiver) rows.push(...flattenAst(node.receiver, depth + 1));
    node.args.forEach((arg) => rows.push(...flattenAst(arg, depth + 1)));
  }
  return rows;
}

export function describeNode(node: CelNode) {
  switch (node.kind) {
    case 'and':
      return 'Logical AND call planned as evalAnd';
    case 'eq':
      return 'Equality call planned as evalEq';
    case 'path':
      return `${node.root}.${node.qualifiers.join('.')}`;
    case 'literal':
      return `${node.valueType} ${formatValue(node.value)}`;
    case 'call':
      return `${node.functionName}() overload ${node.overloadId}`;
  }
}

export function nodeKindTone(node: CelNode): 'cyan' | 'green' | 'amber' | 'rose' | 'slate' {
  if (node.kind === 'and') return 'amber';
  if (node.kind === 'eq') return 'cyan';
  if (node.kind === 'path') return 'green';
  if (node.kind === 'call') return 'rose';
  return 'slate';
}
