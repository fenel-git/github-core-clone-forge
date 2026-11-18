type Dict = Record<string, unknown>;
type RpcId = string | number | null;

export interface BackendResponse {
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
}

/** The same authenticated REST dispatcher used by the HTTP routes. */
export interface Backend {
  viewer: unknown;
  request(method: string, path: string, body?: any): BackendResponse;
}

export interface McpResponse {
  status: number;
  body: any;
  headers?: Record<string, string>;
}

const modernRevision = '2026-07-28';
const legacyRevisions = ['2025-11-25', '2025-06-18'];
const identity = {
  serverInfo: { name: 'github-core-clone', version: '1.0.0' },
  capabilities: { tools: {} },
};
const jsonHeaders = { 'content-type': 'application/json; charset=utf-8' };

interface Property {
  type: 'string' | 'boolean' | 'integer';
  minLength?: number;
  minimum?: number;
  enum?: string[];
}

function schema(properties: Record<string, Property>, required: string[]) {
  return { type: 'object', properties, required };
}

const text = { type: 'string' } as const;
const nonEmpty = { ...text, minLength: 1 };
const repoSegment = { ...nonEmpty, pattern: '^[A-Za-z0-9_.-]+$', not: { enum: ['.', '..'] } };
const repoFields = { owner: repoSegment, repo: repoSegment };
const repoRequired = ['owner', 'repo'];
const tools = [
  {
    name: 'create_repo',
    description: 'Create a repository for the authenticated user.',
    inputSchema: schema({ name: nonEmpty, description: text, private: { type: 'boolean' } }, ['name']),
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: schema(repoFields, repoRequired),
  },
  {
    name: 'create_issue',
    description: 'Open an issue in a repository.',
    inputSchema: schema({ ...repoFields, title: nonEmpty, body: text }, [...repoRequired, 'title']),
  },
  {
    name: 'list_issues',
    description: 'List repository issues, optionally filtered by state.',
    inputSchema: schema({ ...repoFields, state: { ...text, enum: ['open', 'closed', 'all'] } }, repoRequired),
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: schema({ ...repoFields, title: nonEmpty, body: text, head: nonEmpty, base: nonEmpty }, [...repoRequired, 'title', 'head', 'base']),
  },
  {
    name: 'merge_pull_request',
    description: 'Squash and merge a pull request by number.',
    inputSchema: schema({ ...repoFields, pull_number: { type: 'integer', minimum: 1 } }, [...repoRequired, 'pull_number']),
  },
];

function isRecord(value: unknown): value is Dict {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function result(id: RpcId, value: unknown): McpResponse {
  return { status: 200, body: { jsonrpc: '2.0', id, result: value }, headers: jsonHeaders };
}

function error(id: RpcId, code: number, message: string, status = 200): McpResponse {
  return { status, body: { jsonrpc: '2.0', id, error: { code, message } }, headers: jsonHeaders };
}

function validProperty(value: unknown, property: Property): boolean {
  if (property.type === 'integer') {
    return typeof value === 'number' && Number.isSafeInteger(value) && value >= (property.minimum ?? 0);
  }
  if (property.type === 'boolean') return typeof value === 'boolean';
  return typeof value === 'string' && value.length >= (property.minLength ?? 0) &&
    (property.enum === undefined || property.enum.includes(value));
}

function pick(args: Dict, ...keys: string[]): Dict {
  return Object.fromEntries(keys.filter((key) => Object.hasOwn(args, key)).map((key) => [key, args[key]]));
}

function dispatch(api: Backend, name: string, args: Dict): BackendResponse {
  if (name === 'create_repo') return api.request('POST', '/api/v3/user/repos', pick(args, 'name', 'description', 'private'));
  const repo = `/api/v3/repos/${encodeURIComponent(args.owner as string)}/${encodeURIComponent(args.repo as string)}`;
  switch (name) {
    case 'get_repo':
      return api.request('GET', repo);
    case 'create_issue':
      return api.request('POST', `${repo}/issues`, pick(args, 'title', 'body'));
    case 'list_issues':
      return api.request('GET', `${repo}/issues${args.state === undefined ? '' : `?state=${args.state}`}`);
    case 'create_pull_request':
      return api.request('POST', `${repo}/pulls`, pick(args, 'title', 'body', 'head', 'base'));
    case 'merge_pull_request':
      return api.request('PUT', `${repo}/pulls/${args.pull_number}/merge`, { merge_method: 'squash' });
    default:
      throw new Error('Tool has no REST mapping');
  }
}

function toolResult(text: string, data?: Dict, isError = false): Dict {
  return {
    content: [{ type: 'text', text }],
    ...(data === undefined ? {} : { structuredContent: data }),
    ...(isError ? { isError: true } : {}),
  };
}

function callTool(id: RpcId, params: Dict, api: Backend): McpResponse {
  const tool = tools.find(({ name }) => name === params.name);
  if (tool === undefined) return error(id, -32602, 'Missing or unknown tool name');
  const args = params.arguments === undefined ? {} : params.arguments;
  if (!isRecord(args)) return error(id, -32602, 'Tool arguments must be an object');
  for (const [key, property] of Object.entries(tool.inputSchema.properties)) {
    if (!Object.hasOwn(args, key)) {
      if (tool.inputSchema.required.includes(key)) return error(id, -32602, `Missing argument: ${key}`);
    } else if (!validProperty(args[key], property) ||
      ((key === 'owner' || key === 'repo') && !validRepoSegment(args[key]))) {
      return error(id, -32602, `Invalid argument: ${key}`);
    }
  }
  try {
    const answer = dispatch(api, tool.name, args);
    if (answer.status < 200 || answer.status >= 300) {
      const detail = isRecord(answer.body) && typeof answer.body.message === 'string' ? `: ${answer.body.message}` : '';
      return result(id, toolResult(`REST request failed with HTTP ${answer.status}${detail}`, undefined, true));
    }
    // MCP structured content is an object; array responses are serialized in the text block.
    return result(id, toolResult(JSON.stringify(answer.body ?? null), isRecord(answer.body) ? answer.body : undefined));
  } catch {
    return result(id, toolResult('REST backend could not complete the operation', undefined, true));
  }
}

function validRepoSegment(value: unknown): boolean {
  return typeof value === 'string' && new RegExp(repoSegment.pattern).test(value) && !repoSegment.not.enum.includes(value);
}

/** Handles a parsed `/mcp` POST. The HTTP route owns bearer auth and malformed JSON (-32700). */
export function mcp(body: unknown, api: Backend): McpResponse {
  if (!isRecord(body) || body.jsonrpc !== '2.0' || typeof body.method !== 'string' || body.method === '') {
    return error(null, -32600, 'Invalid Request', 400);
  }
  // This stateless server has no notification side effects. Never execute tools without a request ID.
  if (!Object.hasOwn(body, 'id')) return { status: 202, body: null };
  const id = body.id;
  if (id !== null && typeof id !== 'string' && !(typeof id === 'number' && Number.isFinite(id))) {
    return error(null, -32600, 'Invalid Request ID', 400);
  }
  const params = body.params === undefined ? {} : body.params;
  if (!isRecord(params)) return error(id, -32602, 'Params must be an object');

  switch (body.method) {
    case 'server/discover':
      return result(id, {
        ...identity,
        supportedVersions: [modernRevision],
        _meta: { 'io.modelcontextprotocol/serverInfo': identity.serverInfo },
      });
    case 'initialize': {
      const requested = params.protocolVersion;
      const protocolVersion = typeof requested === 'string' && legacyRevisions.includes(requested)
        ? requested : legacyRevisions[0];
      return result(id, { ...identity, protocolVersion });
    }
    case 'ping':
      return result(id, {});
    case 'tools/list':
      return result(id, { tools });
    case 'tools/call':
      return callTool(id, params, api);
    default:
      return error(id, -32601, 'Method not found');
  }
}

export default mcp;
