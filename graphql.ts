export interface Backend {
  viewer: any;
  request(method: string, path: string, body?: any): { status: number; body: any; headers?: Record<string, string> };
  repoById(id: string | number): any | undefined;
  issueById?(id: string | number): any | undefined;
}

type Token = { kind: 'name' | 'string' | 'number' | 'punct' | 'eof'; value: string; offset: number };
type Value = any;
type Directive = { name: string; args: Record<string, Value> };
type Field = { kind: 'field'; name: string; alias: string; args: Record<string, Value>; directives: Directive[]; selections: Selection[] };
type InlineFragment = { kind: 'inline'; typeCondition?: string; directives: Directive[]; selections: Selection[] };
type FragmentSpread = { kind: 'spread'; name: string; directives: Directive[] };
type Selection = Field | InlineFragment | FragmentSpread;
type Operation = { kind: 'query' | 'mutation' | 'subscription'; name?: string; variables: Record<string, Value>; selections: Selection[] };
type Fragment = { typeCondition: string; directives: Directive[]; selections: Selection[] };
type Document = { operations: Operation[]; fragments: Record<string, Fragment> };

class GraphQLError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'GraphQLError';
  }
}

function lex(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < source.length) {
    const ch = source[i] ?? '';
    if (/\s|,|\uFEFF/u.test(ch)) { i += 1; continue; }
    if (ch === '#') {
      while (i < source.length && source[i] !== '\n' && source[i] !== '\r') i += 1;
      continue;
    }
    if (source.startsWith('...', i)) { tokens.push({ kind: 'punct', value: '...', offset: i }); i += 3; continue; }
    if (ch === '"') {
      const start = i;
      if (source.startsWith('"""', i)) {
        i += 3;
        let value = '';
        while (i < source.length && !source.startsWith('"""', i)) {
          if (source.startsWith('\\"""', i)) { value += '"""'; i += 4; }
          else value += source[i++];
        }
        if (!source.startsWith('"""', i)) throw new GraphQLError(`Unterminated block string at offset ${start}`);
        i += 3;
        tokens.push({ kind: 'string', value, offset: start });
      } else {
        i += 1;
        let escaped = false;
        while (i < source.length) {
          const current = source[i++] ?? '';
          if (!escaped && current === '"') break;
          if (!escaped && current === '\\') escaped = true;
          else escaped = false;
        }
        const literal = source.slice(start, i);
        if (!literal.endsWith('"')) throw new GraphQLError(`Unterminated string at offset ${start}`);
        try { tokens.push({ kind: 'string', value: JSON.parse(literal) as string, offset: start }); }
        catch { throw new GraphQLError(`Invalid string at offset ${start}`); }
      }
      continue;
    }
    if (/[A-Za-z_]/u.test(ch)) {
      const start = i++;
      while (i < source.length && /[A-Za-z0-9_]/u.test(source[i] ?? '')) i += 1;
      tokens.push({ kind: 'name', value: source.slice(start, i), offset: start });
      continue;
    }
    if (ch === '-' || /[0-9]/u.test(ch)) {
      const start = i++;
      while (i < source.length && /[0-9.eE+-]/u.test(source[i] ?? '')) i += 1;
      const value = source.slice(start, i);
      if (!/^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$/u.test(value)) {
        throw new GraphQLError(`Invalid number ${JSON.stringify(value)} at offset ${start}`);
      }
      tokens.push({ kind: 'number', value, offset: start });
      continue;
    }
    if ('!$():=@[]{|}&'.includes(ch)) { tokens.push({ kind: 'punct', value: ch, offset: i }); i += 1; continue; }
    throw new GraphQLError(`Unexpected character ${JSON.stringify(ch)} at offset ${i}`);
  }
  tokens.push({ kind: 'eof', value: '', offset: source.length });
  return tokens;
}

class Parser {
  private readonly tokens: Token[];
  private at = 0;

  constructor(source: string) { this.tokens = lex(source); }

  private peek(value?: string): Token {
    const token = this.tokens[this.at] ?? { kind: 'eof', value: '', offset: 0 };
    return value === undefined || token.value === value ? token : token;
  }

  private take(): Token { return this.tokens[this.at++] ?? { kind: 'eof', value: '', offset: 0 }; }

  private expect(value: string): Token {
    const token = this.take();
    if (token.value !== value) throw new GraphQLError(`Expected ${JSON.stringify(value)} at offset ${token.offset}, got ${JSON.stringify(token.value)}`);
    return token;
  }

  private name(): string {
    const token = this.take();
    if (token.kind !== 'name') throw new GraphQLError(`Expected a name at offset ${token.offset}, got ${JSON.stringify(token.value)}`);
    return token.value;
  }

  private typeReference(): void {
    if (this.peek().value === '[') {
      this.take();
      this.typeReference();
      this.expect(']');
    } else this.name();
    if (this.peek().value === '!') this.take();
  }

  private value(): Value {
    const token = this.peek();
    if (token.value === '$') { this.take(); return { __variable: this.name() }; }
    if (token.value === '[') {
      this.take();
      const values: Value[] = [];
      while (this.peek().value !== ']') {
        if (this.peek().kind === 'eof') throw new GraphQLError(`Unterminated list at offset ${token.offset}`);
        values.push(this.value());
      }
      this.take();
      return values;
    }
    if (token.value === '{') {
      this.take();
      const object: Record<string, Value> = Object.create(null) as Record<string, Value>;
      while (this.peek().value !== '}') {
        if (this.peek().kind === 'eof') throw new GraphQLError(`Unterminated input object at offset ${token.offset}`);
        const key = this.name();
        this.expect(':');
        object[key] = this.value();
      }
      this.take();
      return object;
    }
    if (token.kind === 'string') { this.take(); return token.value; }
    if (token.kind === 'number') { this.take(); return Number(token.value); }
    if (token.kind === 'name') {
      this.take();
      if (token.value === 'null') return null;
      if (token.value === 'true') return true;
      if (token.value === 'false') return false;
      return token.value;
    }
    throw new GraphQLError(`Expected a value at offset ${token.offset}`);
  }

  private arguments(): Record<string, Value> {
    const args: Record<string, Value> = Object.create(null) as Record<string, Value>;
    if (this.peek().value !== '(') return args;
    this.take();
    while (this.peek().value !== ')') {
      if (this.peek().kind === 'eof') throw new GraphQLError('Unterminated arguments');
      const key = this.name();
      this.expect(':');
      args[key] = this.value();
    }
    this.take();
    return args;
  }

  private directives(): Directive[] {
    const directives: Directive[] = [];
    while (this.peek().value === '@') {
      this.take();
      directives.push({ name: this.name(), args: this.arguments() });
    }
    return directives;
  }

  private variableDefinitions(): Record<string, Value> {
    const defaults: Record<string, Value> = Object.create(null) as Record<string, Value>;
    if (this.peek().value !== '(') return defaults;
    this.take();
    while (this.peek().value !== ')') {
      if (this.peek().kind === 'eof') throw new GraphQLError('Unterminated variable definitions');
      this.expect('$');
      const name = this.name();
      this.expect(':');
      this.typeReference();
      if (this.peek().value === '=') { this.take(); defaults[name] = this.value(); }
      this.directives();
    }
    this.take();
    return defaults;
  }

  private selectionSet(): Selection[] {
    this.expect('{');
    const selections: Selection[] = [];
    while (this.peek().value !== '}') {
      if (this.peek().kind === 'eof') throw new GraphQLError('Unterminated selection set');
      selections.push(this.selection());
    }
    this.take();
    return selections;
  }

  private selection(): Selection {
    if (this.peek().value === '...') {
      this.take();
      if (this.peek().value === 'on') {
        this.take();
        const typeCondition = this.name();
        const directives = this.directives();
        return { kind: 'inline', typeCondition, directives, selections: this.selectionSet() };
      }
      if (this.peek().value === '@' || this.peek().value === '{') {
        const directives = this.directives();
        return { kind: 'inline', directives, selections: this.selectionSet() };
      }
      const name = this.name();
      return { kind: 'spread', name, directives: this.directives() };
    }
    const first = this.name();
    let name = first;
    let alias = first;
    if (this.peek().value === ':') { this.take(); alias = first; name = this.name(); }
    const args = this.arguments();
    const directives = this.directives();
    const selections = this.peek().value === '{' ? this.selectionSet() : [];
    return { kind: 'field', name, alias, args, directives, selections };
  }

  parse(): Document {
    const operations: Operation[] = [];
    const fragments: Record<string, Fragment> = Object.create(null) as Record<string, Fragment>;
    while (this.peek().kind !== 'eof') {
      if (this.peek().value === '{') {
        operations.push({ kind: 'query', variables: Object.create(null) as Record<string, Value>, selections: this.selectionSet() });
        continue;
      }
      const definition = this.name();
      if (definition === 'fragment') {
        const name = this.name();
        this.expect('on');
        const typeCondition = this.name();
        const directives = this.directives();
        fragments[name] = { typeCondition, directives, selections: this.selectionSet() };
        continue;
      }
      if (definition !== 'query' && definition !== 'mutation' && definition !== 'subscription') {
        throw new GraphQLError(`Unexpected definition ${JSON.stringify(definition)}`);
      }
      const name = this.peek().kind === 'name' ? this.name() : undefined;
      const variables = this.variableDefinitions();
      this.directives();
      operations.push({ kind: definition, ...(name === undefined ? {} : { name }), variables, selections: this.selectionSet() });
    }
    if (operations.length === 0) throw new GraphQLError('Document contains no operation');
    return { operations, fragments };
  }
}

function resolveValue(value: Value, variables: Record<string, Value>): Value {
  if (Array.isArray(value)) return value.map((item) => resolveValue(item, variables));
  if (value !== null && typeof value === 'object') {
    if (typeof value.__variable === 'string') return variables[value.__variable];
    const result: Record<string, Value> = Object.create(null) as Record<string, Value>;
    for (const [key, item] of Object.entries(value)) result[key] = resolveValue(item, variables);
    return result;
  }
  return value;
}

function isRecord(value: any): value is Record<string, any> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function encodeId(payload: Record<string, unknown>): string {
  return `gql1_${Buffer.from(JSON.stringify(payload), 'utf8').toString('base64url')}`;
}

function decodeId(id: unknown): Record<string, any> | null {
  if (typeof id !== 'string' || !id.startsWith('gql1_')) return null;
  try {
    const value: unknown = JSON.parse(Buffer.from(id.slice(5), 'base64url').toString('utf8'));
    return isRecord(value) && value.version === 1 && typeof value.type === 'string' ? value : null;
  } catch { return null; }
}

function graphId(type: string, record: any, locator: Record<string, unknown> = {}): string {
  return encodeId({ version: 1, type, nodeId: record?.node_id ?? null, databaseId: record?.id ?? null, ...locator });
}

function repoPath(owner: string, name: string): string {
  return `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(name)}`;
}

function withSearch(path: string, params: Record<string, unknown>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') search.set(key, String(value));
  }
  const query = search.toString();
  return query === '' ? path : `${path}?${query}`;
}

function request(api: Backend, method: string, path: string, body?: any, allowNotFound = false): any | null {
  let response: { status: number; body: any };
  try { response = api.request(method, path, body); }
  catch (error) { throw new GraphQLError(error instanceof Error ? error.message : String(error)); }
  if (allowNotFound && response.status === 404) return null;
  if (response.status < 200 || response.status >= 300) {
    const message = isRecord(response.body) && typeof response.body.message === 'string' ? response.body.message : `HTTP ${response.status}`;
    throw new GraphQLError(message);
  }
  return response.body;
}

function getRepo(api: Backend, owner: string, name: string): any | null {
  return request(api, 'GET', repoPath(owner, name), undefined, true);
}

function repoFromId(api: Backend, id: unknown): any | null {
  const decoded = decodeId(id);
  if (decoded !== null && decoded.type === 'Repository') {
    const candidates: Array<string | number> = [];
    if (typeof decoded.nodeId === 'string') candidates.push(decoded.nodeId);
    if (typeof decoded.databaseId === 'string' || typeof decoded.databaseId === 'number') candidates.push(decoded.databaseId);
    for (const candidate of candidates) {
      try { const found = api.repoById(candidate); if (found !== undefined && found !== null) return found; }
      catch { /* Try the other stable REST identifier. */ }
    }
    if (typeof decoded.owner === 'string' && typeof decoded.name === 'string') return getRepo(api, decoded.owner, decoded.name);
    return null;
  }
  if (typeof id !== 'string' && typeof id !== 'number') return null;
  try { return api.repoById(id) ?? null; } catch { return null; }
}

function locatorForRepo(repo: any, ownerHint?: string, nameHint?: string): { owner: string; name: string } {
  const owner = typeof repo?.owner === 'string' ? repo.owner : repo?.owner?.login;
  const fullName = typeof repo?.full_name === 'string' ? repo.full_name : repo?.nameWithOwner;
  const fullParts = typeof fullName === 'string' ? fullName.split('/') : [];
  const name = typeof repo?.name === 'string' ? repo.name : fullParts.at(-1);
  return { owner: owner ?? fullParts[0] ?? ownerHint ?? '', name: name ?? nameHint ?? '' };
}

function issueFromId(api: Backend, id: unknown): { issue: any | null; owner: string; repo: string; number: number | null } {
  const decoded = decodeId(id);
  let issue: any | null = null;
  if (decoded !== null && decoded.type === 'Issue') {
    if (typeof decoded.nodeId === 'string' && typeof api.issueById === 'function') {
      try { issue = api.issueById(decoded.nodeId) ?? null; } catch { issue = null; }
    }
    const owner = typeof decoded.owner === 'string' ? decoded.owner : '';
    const repo = typeof decoded.repo === 'string' ? decoded.repo : '';
    const number = typeof decoded.number === 'number' ? decoded.number : null;
    if (issue === null && owner !== '' && repo !== '' && number !== null) {
      issue = request(api, 'GET', `${repoPath(owner, repo)}/issues/${number}`, undefined, true);
    }
    return { issue, owner, repo, number };
  }
  if (typeof id === 'string' || typeof id === 'number') {
    if (typeof api.issueById === 'function') {
      try { issue = api.issueById(id) ?? null; } catch { issue = null; }
    }
  }
  return { issue, owner: '', repo: '', number: typeof issue?.number === 'number' ? issue.number : null };
}

function pullRequestFromId(api: Backend, id: unknown): { pullRequest: any | null; owner: string; repo: string; number: number | null } {
  const decoded = decodeId(id);
  if (decoded !== null && decoded.type === 'PullRequest') {
    const owner = typeof decoded.owner === 'string' ? decoded.owner : '';
    const repo = typeof decoded.repo === 'string' ? decoded.repo : '';
    const number = typeof decoded.number === 'number' ? decoded.number : null;
    const pullRequest = owner !== '' && repo !== '' && number !== null
      ? request(api, 'GET', `${repoPath(owner, repo)}/pulls/${number}`, undefined, true)
      : null;
    return { pullRequest, owner, repo, number };
  }
  return { pullRequest: null, owner: '', repo: '', number: null };
}

function listBody(api: Backend, path: string): any[] {
  const body = request(api, 'GET', path);
  if (!Array.isArray(body)) return [];
  return body;
}

function toUpperState(state: unknown): string | null {
  if (typeof state !== 'string') return null;
  return state.toLowerCase() === 'open' ? 'OPEN' : state.toLowerCase() === 'closed' ? 'CLOSED' : state.toUpperCase();
}

function typeFields(name: string): string[] | null {
  const fields: Record<string, string[]> = {
    Issue: ['id', 'number', 'title', 'state', 'body'],
    PullRequest: ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files'],
    Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  };
  return fields[name] ?? null;
}

const ENUMS: Record<string, string[]> = {
  IssueState: ['OPEN', 'CLOSED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
};

function introspectionType(name: unknown): any | null {
  if (typeof name !== 'string') return null;
  const fields = typeFields(name);
  if (fields !== null) return { name, fields: fields.map((field) => ({ name: field })) };
  const values = ENUMS[name];
  if (values !== undefined) return { name, enumValues: values.map((value) => ({ name: value })) };
  if (['Query', 'Mutation', 'User', 'IssueConnection', 'PullRequestConnection', 'PageInfo', 'PullRequestChangedFile'].includes(name)) {
    const generic: Record<string, string[]> = {
      Query: ['viewer', 'repository', 'node', '__type'],
      Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
      User: ['login'],
      IssueConnection: ['nodes', 'pageInfo'],
      PullRequestConnection: ['nodes'],
      PageInfo: ['hasNextPage', 'endCursor'],
      PullRequestChangedFile: ['path'],
    };
    return { name, fields: (generic[name] ?? []).map((field) => ({ name: field })) };
  }
  return null;
}

function encodeCursor(offset: number): string {
  return Buffer.from(`issues:${offset}`, 'utf8').toString('base64url');
}

function decodeCursor(cursor: unknown): number {
  if (typeof cursor !== 'string' || cursor === '') return 0;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const match = /^issues:(\d+)$/u.exec(text);
    return match === null ? 0 : Math.max(0, Number(match[1]));
  } catch { return 0; }
}

function issueConnection(api: Backend, repo: any, owner: string, name: string, args: Record<string, any>): any {
  const orderBy = isRecord(args.orderBy) ? args.orderBy : {};
  const direction = typeof orderBy.direction === 'string' ? orderBy.direction.toLowerCase() : 'desc';
  const first = typeof args.first === 'number' && Number.isFinite(args.first) ? Math.max(0, Math.trunc(args.first)) : 30;
  const states = Array.isArray(args.states) ? args.states.map((state: unknown) => String(state).toUpperCase()) : [];
  const params: Record<string, unknown> = { per_page: 100, page: 1 };
  if (states.length === 1) params.state = states[0] === 'OPEN' ? 'open' : states[0] === 'CLOSED' ? 'closed' : 'all';
  else if (states.length === 0 || states.includes('OPEN') && states.includes('CLOSED')) params.state = 'all';
  else if (states.includes('OPEN')) params.state = 'open';
  else if (states.includes('CLOSED')) params.state = 'closed';
  if (orderBy.field === 'CREATED_AT' || orderBy.field === 'UPDATED_AT') {
    params.sort = orderBy.field === 'UPDATED_AT' ? 'updated' : 'created';
    params.direction = direction === 'asc' ? 'asc' : 'desc';
  }
  let items = listBody(api, withSearch(`${repoPath(owner, name)}/issues`, params)).filter((item) => !item.pull_request);
  if (states.length > 0) items = items.filter((item) => states.includes(toUpperState(item.state) ?? ''));
  if (orderBy.field === 'CREATED_AT' || orderBy.field === 'UPDATED_AT') {
    const dateKey = orderBy.field === 'UPDATED_AT' ? 'updated_at' : 'created_at';
    const sign = direction === 'asc' ? 1 : -1;
    items.sort((a, b) => {
      const left = Date.parse(String(a[dateKey] ?? '')) || 0;
      const right = Date.parse(String(b[dateKey] ?? '')) || 0;
      return sign * (left - right) || sign * ((Number(a.number) || 0) - (Number(b.number) || 0));
    });
  }
  const start = decodeCursor(args.after);
  const nodes = items.slice(start, start + first);
  const end = start + nodes.length;
  return { nodes, pageInfo: { hasNextPage: end < items.length, endCursor: nodes.length === 0 ? null : encodeCursor(end) }, __owner: owner, __repo: name };
}

function mergeableState(pr: any): string {
  if (pr?.mergeable === true || pr?.mergeable_state === 'clean' || pr?.mergeable_state === 'has_hooks') return 'MERGEABLE';
  if (pr?.mergeable === false || ['dirty', 'behind', 'blocked'].includes(String(pr?.mergeable_state ?? '').toLowerCase())) return 'CONFLICTING';
  return 'UNKNOWN';
}

function childType(parentType: string, field: string): string | null {
  const map: Record<string, Record<string, string>> = {
    Mutation: { createIssue: 'CreateIssuePayload', closeIssue: 'CloseIssuePayload', createPullRequest: 'CreatePullRequestPayload', mergePullRequest: 'MergePullRequestPayload', addComment: 'AddCommentPayload' },
    Query: { viewer: 'User', repository: 'Repository', node: 'Node', __type: '__Type' },
    Repository: { owner: 'User', defaultBranchRef: 'Ref', issue: 'Issue', issues: 'IssueConnection', pullRequests: 'PullRequestConnection', pullRequest: 'PullRequest' },
    IssueConnection: { nodes: 'Issue', pageInfo: 'PageInfo' },
    PullRequestConnection: { nodes: 'PullRequest' },
    PullRequest: { files: 'PullRequestFileConnection' },
    PullRequestFileConnection: { nodes: 'PullRequestChangedFile' },
    __Type: { fields: '__Field', enumValues: '__EnumValue' },
    CreateIssuePayload: { issue: 'Issue' },
    CloseIssuePayload: { issue: 'Issue' },
    CreatePullRequestPayload: { pullRequest: 'PullRequest' },
    MergePullRequestPayload: { pullRequest: 'PullRequest' },
    AddCommentPayload: { commentEdge: 'CommentEdge' },
    CommentEdge: { node: 'Comment' },
  };
  return map[parentType]?.[field] ?? null;
}

function allowedFields(type: string): string[] | null {
  const fields: Record<string, string[]> = {
    Query: ['viewer', 'repository', 'node', '__type'],
    Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
    Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
    User: ['login', 'id', 'name'], Ref: ['name'],
    Issue: ['id', 'number', 'title', 'state', 'body'],
    IssueConnection: ['nodes', 'pageInfo'], PageInfo: ['hasNextPage', 'endCursor'],
    PullRequestConnection: ['nodes'],
    PullRequest: ['id', 'number', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files'],
    PullRequestFileConnection: ['nodes'], PullRequestChangedFile: ['path'],
    __Type: ['name', 'fields', 'enumValues'], __Field: ['name'], __EnumValue: ['name'],
    CreateIssuePayload: ['issue'], CloseIssuePayload: ['issue'], CreatePullRequestPayload: ['pullRequest'],
    MergePullRequestPayload: ['pullRequest'], AddCommentPayload: ['commentEdge'], CommentEdge: ['node'], Comment: ['id', 'body'],
  };
  if (fields[type] !== undefined) return fields[type];
  if (ENUMS[type] !== undefined) return [];
  return null;
}

function fieldValue(type: string, parent: any, field: Field, env: ExecEnv): any {
  const args: Record<string, any> = Object.create(null) as Record<string, any>;
  for (const [key, value] of Object.entries(field.args)) args[key] = resolveValue(value, env.variables);
  const api = env.api;
  switch (type) {
    case 'Query':
      if (field.name === 'viewer') return api.viewer;
      if (field.name === 'repository') {
        if (typeof args.owner !== 'string' || typeof args.name !== 'string') throw new GraphQLError('repository requires owner and name');
        return getRepo(api, args.owner, args.name);
      }
      if (field.name === 'node') return nodeById(api, args.id);
      if (field.name === '__type') return introspectionType(args.name);
      break;
    case 'User':
      if (field.name === 'login') return parent?.login;
      if (field.name === 'id') return parent?.id ?? null;
      if (field.name === 'name') return parent?.name ?? null;
      break;
    case 'Ref':
      if (field.name === 'name') return parent?.name ?? null;
      break;
    case 'Repository': {
      const locator = locatorForRepo(parent);
      switch (field.name) {
        case 'id': return graphId('Repository', parent, locator);
        case 'databaseId': return parent?.id ?? parent?.databaseId ?? null;
        case 'name': return parent?.name ?? null;
        case 'nameWithOwner': return parent?.full_name ?? parent?.nameWithOwner ?? `${locator.owner}/${locator.name}`;
        case 'owner': return typeof parent?.owner === 'string' ? { login: parent.owner } : parent?.owner ?? { login: locator.owner };
        case 'defaultBranchRef': {
          const branch = parent?.default_branch ?? parent?.defaultBranch ?? null;
          return branch === null ? null : { name: branch };
        }
        case 'viewerPermission': return parent?.viewerPermission ?? (parent?.permissions?.admin ? 'ADMIN' : 'ADMIN');
        case 'hasIssuesEnabled': return parent?.has_issues ?? parent?.hasIssuesEnabled ?? true;
        case 'issue': {
          if (typeof args.number !== 'number') throw new GraphQLError('issue requires number');
          return request(api, 'GET', `${repoPath(locator.owner, locator.name)}/issues/${args.number}`, undefined, true);
        }
        case 'issues': return issueConnection(api, parent, locator.owner, locator.name, args);
        case 'pullRequests': {
          const first = typeof args.first === 'number' ? Math.max(0, Math.trunc(args.first)) : 30;
          const pulls = listBody(api, withSearch(`${repoPath(locator.owner, locator.name)}/pulls`, { state: 'open', per_page: Math.max(first, 1), page: 1 }));
          return { nodes: pulls.slice(0, first), __owner: locator.owner, __repo: locator.name };
        }
        case 'pullRequest': {
          if (typeof args.number !== 'number') throw new GraphQLError('pullRequest requires number');
          return request(api, 'GET', `${repoPath(locator.owner, locator.name)}/pulls/${args.number}`, undefined, true);
        }
      }
      break;
    }
    case 'Issue':
      switch (field.name) {
        case 'id': return graphId('Issue', parent, { owner: env.owner ?? '', repo: env.repo ?? '', number: parent?.number ?? null });
        case 'number': return parent?.number ?? null;
        case 'title': return parent?.title ?? null;
        case 'state': return toUpperState(parent?.state);
        case 'body': return parent?.body ?? null;
      }
      break;
    case 'IssueConnection':
      if (field.name === 'nodes') return parent?.nodes ?? [];
      if (field.name === 'pageInfo') return parent?.pageInfo ?? { hasNextPage: false, endCursor: null };
      break;
    case 'PageInfo':
      if (field.name === 'hasNextPage') return parent?.hasNextPage ?? false;
      if (field.name === 'endCursor') return parent?.endCursor ?? null;
      break;
    case 'PullRequestConnection':
      if (field.name === 'nodes') return parent?.nodes ?? [];
      break;
    case 'PullRequest':
      switch (field.name) {
        case 'id': return graphId('PullRequest', parent, { owner: env.owner ?? '', repo: env.repo ?? '', number: parent?.number ?? null });
        case 'number': return parent?.number ?? null;
        case 'state': return parent?.merged === true ? 'MERGED' : toUpperState(parent?.state);
        case 'merged': return parent?.merged === true;
        case 'headRefName': return parent?.head?.ref ?? parent?.headRefName ?? null;
        case 'baseRefName': return parent?.base?.ref ?? parent?.baseRefName ?? null;
        case 'mergeable': return mergeableState(parent);
        case 'files': {
          const locator = { owner: env.owner ?? parent?.__owner ?? '', name: env.repo ?? parent?.__repo ?? '' };
          const number = parent?.number;
          if (locator.owner === '' || locator.name === '' || typeof number !== 'number') return { nodes: [] };
          const first = typeof args.first === 'number' ? Math.max(0, Math.trunc(args.first)) : 30;
          const files = listBody(api, withSearch(`${repoPath(locator.owner, locator.name)}/pulls/${number}/files`, { per_page: Math.max(first, 1), page: 1 }));
          return { nodes: files.slice(0, first) };
        }
      }
      break;
    case 'PullRequestFileConnection':
      if (field.name === 'nodes') return parent?.nodes ?? [];
      break;
    case 'PullRequestChangedFile':
      if (field.name === 'path') return parent?.filename ?? parent?.path ?? null;
      break;
    case '__Type':
      if (field.name === 'name') return parent?.name ?? null;
      if (field.name === 'fields') return parent?.fields ?? null;
      if (field.name === 'enumValues') return parent?.enumValues ?? null;
      break;
    case '__Field': case '__EnumValue':
      if (field.name === 'name') return parent?.name ?? null;
      break;
    case 'CreateIssuePayload': case 'CloseIssuePayload':
      if (field.name === 'issue') return parent?.issue ?? null;
      break;
    case 'CreatePullRequestPayload': case 'MergePullRequestPayload':
      if (field.name === 'pullRequest') return parent?.pullRequest ?? null;
      break;
    case 'AddCommentPayload':
      if (field.name === 'commentEdge') return parent?.commentEdge ?? null;
      break;
    case 'CommentEdge':
      if (field.name === 'node') return parent?.node ?? null;
      break;
    case 'Comment':
      if (field.name === 'id') return graphId('Comment', parent);
      if (field.name === 'body') return parent?.body ?? null;
      break;
  }
  if (type === 'Mutation') return mutationValue(api, field.name, args);
  if (field.name === '__typename') return type;
  throw new GraphQLError(`Cannot query field "${field.name}" on type "${type}".`);
}

type ExecEnv = { api: Backend; variables: Record<string, Value>; fragments: Record<string, Fragment>; owner?: string; repo?: string };

function nodeById(api: Backend, id: unknown): any | null {
  const decoded = decodeId(id);
  if (decoded !== null) {
    if (decoded.type === 'Repository') return repoFromId(api, id);
    if (decoded.type === 'Issue') return issueFromId(api, id).issue;
    if (decoded.type === 'PullRequest') return pullRequestFromId(api, id).pullRequest;
  }
  if (typeof id === 'string' || typeof id === 'number') {
    try { const repo = api.repoById(id); if (repo !== undefined && repo !== null) return repo; } catch { /* Try an issue id below. */ }
    if (typeof api.issueById === 'function') {
      try { return api.issueById(id) ?? null; } catch { return null; }
    }
  }
  return null;
}

function mutationValue(api: Backend, name: string, args: Record<string, any>): any {
  const input = isRecord(args.input) ? args.input : {};
  switch (name) {
    case 'createIssue': {
      const repo = repoFromId(api, input.repositoryId);
      if (repo === null) throw new GraphQLError('createIssue could not find the repository');
      const loc = locatorForRepo(repo);
      const issue = request(api, 'POST', `${repoPath(loc.owner, loc.name)}/issues`, { title: input.title, body: input.body ?? '' });
      return { issue };
    }
    case 'closeIssue': {
      const located = issueFromId(api, input.issueId);
      if (located.issue === null || located.owner === '' || located.repo === '' || located.number === null) throw new GraphQLError('closeIssue could not find the issue');
      const issue = request(api, 'PATCH', `${repoPath(located.owner, located.repo)}/issues/${located.number}`, { state: 'closed' });
      return { issue };
    }
    case 'createPullRequest': {
      const repo = repoFromId(api, input.repositoryId);
      if (repo === null) throw new GraphQLError('createPullRequest could not find the repository');
      const loc = locatorForRepo(repo);
      const pullRequest = request(api, 'POST', `${repoPath(loc.owner, loc.name)}/pulls`, {
        title: input.title,
        head: input.headRefName,
        base: input.baseRefName,
      });
      return { pullRequest };
    }
    case 'mergePullRequest': {
      const located = pullRequestFromId(api, input.pullRequestId);
      if (located.pullRequest === null || located.owner === '' || located.repo === '' || located.number === null) throw new GraphQLError('mergePullRequest could not find the pull request');
      const result = request(api, 'PUT', `${repoPath(located.owner, located.repo)}/pulls/${located.number}/merge`, {
        merge_method: typeof input.mergeMethod === 'string' ? input.mergeMethod.toLowerCase() : 'merge',
      });
      return { pullRequest: { ...located.pullRequest, merged: result?.merged === true, state: result?.merged === true ? 'closed' : located.pullRequest.state } };
    }
    case 'addComment': {
      const located = issueFromId(api, input.subjectId);
      if (located.issue === null || located.owner === '' || located.repo === '' || located.number === null) throw new GraphQLError('addComment could not find the issue');
      const comment = request(api, 'POST', `${repoPath(located.owner, located.repo)}/issues/${located.number}/comments`, { body: input.body });
      return { commentEdge: { node: comment } };
    }
    default: throw new GraphQLError(`Cannot query field "${name}" on type "Mutation".`);
  }
}

function includeSelection(directives: Directive[], variables: Record<string, Value>): boolean {
  for (const directive of directives) {
    const value = resolveValue(directive.args.if, variables);
    if (directive.name === 'skip' && value === true) return false;
    if (directive.name === 'include' && value === false) return false;
  }
  return true;
}

function runtimeType(type: string, value: any): string {
  if (type === 'Node') {
    const decoded = decodeId(value?.id);
    if (decoded?.type === 'Repository') return 'Repository';
    if (decoded?.type === 'PullRequest') return 'PullRequest';
    if (decoded?.type === 'Issue') return 'Issue';
    if (value?.full_name || value?.nameWithOwner) return 'Repository';
    if (value?.head || value?.headRefName) return 'PullRequest';
    if (typeof value?.number === 'number') return 'Issue';
  }
  return type;
}

function project(value: any, type: string, selections: Selection[], env: ExecEnv): any {
  if (value === null || value === undefined) return null;
  const actualType = runtimeType(type, value);
  if (Array.isArray(value)) return value.map((item) => project(item, actualType, selections, env));
  const result: Record<string, any> = {};
  const visit = (selection: Selection[], currentType: string): void => {
    for (const item of selection) {
      if (!includeSelection(item.directives, env.variables)) continue;
      if (item.kind === 'inline') {
        if (item.typeCondition === undefined || item.typeCondition === currentType || item.typeCondition === 'Node') visit(item.selections, currentType);
        continue;
      }
      if (item.kind === 'spread') {
        const fragment = env.fragments[item.name];
        if (fragment === undefined) throw new GraphQLError(`Unknown fragment "${item.name}".`);
        if (includeSelection(fragment.directives, env.variables) && (fragment.typeCondition === currentType || fragment.typeCondition === 'Node')) visit(fragment.selections, currentType);
        continue;
      }
      if (item.name === '__typename') { result[item.alias] = currentType; continue; }
      const known = allowedFields(currentType);
      if (known === null || !known.includes(item.name)) throw new GraphQLError(`Cannot query field "${item.name}" on type "${currentType}".`);
      const nextType = childType(currentType, item.name);
      if (nextType !== null && item.selections.length === 0) throw new GraphQLError(`Field "${item.name}" of type "${nextType}" must have a selection of subfields.`);
      if (nextType === null && item.selections.length > 0) throw new GraphQLError(`Field "${item.name}" must not have a selection since type "${currentType}" has no subfields.`);
      const child = fieldValue(currentType, value, item, { ...env, owner: (value?.__owner ?? env.owner), repo: (value?.__repo ?? env.repo) });
      result[item.alias] = nextType === null ? child : project(child, nextType, item.selections, {
        ...env,
        owner: typeof child?.owner === 'string' ? child.owner : child?.owner?.login ?? value?.__owner ?? (currentType === 'Repository' ? locatorForRepo(value).owner : env.owner),
        repo: typeof child?.name === 'string' && currentType === 'Repository' ? child.name : value?.__repo ?? (currentType === 'Repository' ? locatorForRepo(value).name : env.repo),
      });
    }
  };
  visit(selections, actualType);
  return result;
}

function executeOne(body: any, api: Backend): any {
  if (!isRecord(body) || typeof body.query !== 'string') return { data: null, errors: [{ message: 'Request must contain a GraphQL query string.' }] };
  try {
    const document = new Parser(body.query).parse();
    const operationName = typeof body.operationName === 'string' ? body.operationName : undefined;
    let operation: Operation | undefined;
    if (operationName === undefined) {
      if (document.operations.length !== 1) throw new GraphQLError('Must provide operationName when a document contains multiple operations.');
      operation = document.operations[0];
    } else operation = document.operations.find((candidate) => candidate.name === operationName);
    if (operation === undefined) throw new GraphQLError(`Unknown operation named ${JSON.stringify(operationName)}.`);
    if (operation.kind === 'subscription') throw new GraphQLError('Subscriptions are not supported.');
    const provided = isRecord(body.variables) ? body.variables as Record<string, Value> : {};
    const variables: Record<string, Value> = Object.create(null) as Record<string, Value>;
    for (const [name, defaultValue] of Object.entries(operation.variables)) variables[name] = resolveValue(defaultValue, provided);
    for (const [name, value] of Object.entries(provided)) variables[name] = value;
    const rootType = operation.kind === 'mutation' ? 'Mutation' : 'Query';
    const data = project({}, rootType, operation.selections, { api, variables, fragments: document.fragments });
    return { data };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    return { data: null, errors: [{ message }] };
  }
}

/** Execute one GraphQL request or GitHub-style operation batch against the REST-backed forge. */
export function graphql(body: any, api: Backend, origin: string): any {
  void origin;
  if (Array.isArray(body)) return body.map((operation) => executeOne(operation, api));
  return executeOne(body, api);
}
