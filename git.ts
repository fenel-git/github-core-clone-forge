import { createHash } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { deflate, deflateSync, inflate, inflateSync } from 'node:zlib';

export type GitType = 'blob' | 'tree' | 'commit' | 'tag';
export type GitObject = { type: GitType; data: Buffer };
export type GitFile = { sha: string; mode: string };
type Link = { sha: string; type?: GitType };
type Lookup = (sha: string) => GitObject | undefined;

const ZERO = '0'.repeat(40);
const OID = /^[0-9a-f]{40}$/;
const TYPES = [undefined, 'commit', 'tree', 'blob', 'tag'] as const;
const HEAD = 'refs/heads/main';
const FLUSH = Buffer.from('0000');
const DELIM = Buffer.from('0001');
const MAX_PACKET = 65520;
const MAX_OBJECT = 64 * 1024 * 1024;
const MAX_EXPANDED_PACK = 256 * 1024 * 1024;
const MAX_ENTRIES = 500_000;
const PACK_MAGIC = Buffer.from('PACK');

function sha1(bytes: Buffer): Buffer {
  return createHash('sha1').update(bytes).digest();
}

export function hashObject(type: GitType, data: Buffer): string {
  return createHash('sha1').update(`${type} ${data.length}\0`).update(data).digest('hex');
}

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

function readOid(sha: string | undefined): string {
  assert(sha && OID.test(sha) && sha !== ZERO, 'invalid object ID');
  return sha;
}

function splitHeaders(data: Buffer) {
  const split = data.indexOf('\n\n');
  assert(split !== -1, 'missing object header separator');
  return { headers: data.toString('utf8', 0, split).split('\n'), message: data.toString('utf8', split + 2) };
}

function commitParts(data: Buffer) {
  const { headers, message } = splitHeaders(data);
  assert(headers[0].startsWith('tree '), 'missing commit tree');
  return {
    tree: readOid(headers[0].slice(5)),
    parents: headers.filter(line => line.startsWith('parent ')).map(line => readOid(line.slice(7))),
    message,
  };
}

function tagTarget(data: Buffer): Link {
  const { headers } = splitHeaders(data);
  assert(headers[0]?.startsWith('object ') && headers[1]?.startsWith('type '), 'invalid tag headers');
  const type = headers[1].slice(5) as GitType;
  assert(TYPES.includes(type), 'invalid tag target type');
  return { sha: readOid(headers[0].slice(7)), type };
}

function validName(name: Buffer): boolean {
  return name.length > 0 && !name.includes(0) && !name.includes(47) &&
    !['.', '..', '.git'].includes(name.toString().toLowerCase());
}

function treeEntries(data: Buffer): Array<GitFile & { name: Buffer }> {
  const entries: Array<GitFile & { name: Buffer }> = [];
  for (let p = 0; p < data.length;) {
    const space = data.indexOf(32, p);
    const nul = data.indexOf(0, p);
    assert(space > p && nul > space && nul + 21 <= data.length, 'truncated tree entry');
    const mode = data.toString('ascii', p, space);
    const name = data.subarray(space + 1, nul);
    assert(/^(40000|100644|100755|120000|160000)$/.test(mode) && validName(name), 'invalid tree entry');
    entries.push({ mode, name, sha: readOid(data.toString('hex', nul + 1, nul + 21)) });
    p = nul + 21;
  }
  return entries;
}

function links(object: GitObject, cutParents = false): Link[] {
  switch (object.type) {
    case 'blob': return [];
    case 'tag': return [tagTarget(object.data)];
    case 'tree': return treeEntries(object.data).filter(e => e.mode !== '160000').map(e => ({ sha: e.sha, type: e.mode === '40000' ? 'tree' : 'blob' }));
    case 'commit': {
      const { tree, parents } = commitParts(object.data);
      return [{ sha: tree, type: 'tree' }, ...(cutParents ? [] : parents.map(sha => ({ sha, type: 'commit' as const })))];
    }
  }
}

function walk(roots: Iterable<string>, lookup: Lookup, cut = new Set<string>(), omit = new Set<string>()): Set<string> {
  const seen = new Set<string>();
  const todo: Link[] = Array.from(roots, sha => ({ sha }));
  while (todo.length) {
    const { sha, type } = todo.pop()!;
    const object = lookup(sha);
    assert(object && (!type || object.type === type), 'missing or wrong-type object');
    if (omit.has(sha) || seen.has(sha)) continue;
    seen.add(sha);
    for (const edge of links(object, cut.has(sha))) todo.push(edge);
  }
  return seen;
}

function peel(sha: string, lookup: Lookup): string | undefined {
  const start = sha;
  const seen = new Set<string>();
  let object = lookup(sha);
  while (object?.type === 'tag') {
    assert(!seen.has(sha), 'tag cycle');
    seen.add(sha);
    sha = tagTarget(object.data).sha;
    object = lookup(sha);
    assert(object, 'missing tag target');
  }
  return sha === start ? undefined : sha;
}

function ancestorOf(ancestor: string, descendant: string, lookup: Lookup): boolean {
  const pending = [descendant];
  const seen = new Set<string>();
  while (pending.length) {
    const sha = pending.pop()!;
    if (seen.has(sha)) continue;
    seen.add(sha);
    const object = lookup(sha);
    if (object?.type !== 'commit') continue;
    if (sha === ancestor) return true;
    for (const parent of commitParts(object.data).parents) pending.push(parent);
  }
  return false;
}

class Cursor {
  readonly data: Buffer;
  readonly end: number;
  p: number;
  constructor(data: Buffer, start = 0, end = data.length) {
    this.data = data;
    this.end = end;
    this.p = start;
  }
  take(length: number): Buffer {
    assert(length >= 0 && length <= this.end - this.p, 'truncated data');
    const result = this.data.subarray(this.p, this.p + length);
    this.p += length;
    return result;
  }
  byte(): number {
    assert(this.p < this.end, 'truncated data');
    return this.data[this.p++];
  }
  varint(low = 0, scale = 1, more = true): number {
    while (more) {
      const next = this.byte();
      low += (next & 127) * scale;
      assert(low <= MAX_OBJECT && scale <= MAX_OBJECT, 'object too large');
      scale *= 128;
      more = (next & 128) !== 0;
    }
    return low;
  }
}

function applyDelta(base: Buffer, delta: Buffer, resultSize: number, start: number): Buffer {
  const src = new Cursor(delta, start);
  const result = Buffer.allocUnsafe(resultSize);
  let p = 0;
  while (src.p < src.end) {
    const op = src.byte();
    if (op & 128) {
      let offset = 0;
      let size = 0;
      for (let i = 0; i < 4; i++) if (op & (1 << i)) offset += src.byte() * 256 ** i;
      for (let i = 0; i < 3; i++) if (op & (16 << i)) size += src.byte() * 256 ** i;
      if (size === 0) size = 65536;
      assert(offset + size <= base.length && p + size <= result.length, 'delta copy out of range');
      base.copy(result, p, offset, offset + size);
      p += size;
    } else {
      assert(op > 0 && p + op <= result.length, 'bad delta insertion');
      src.take(op).copy(result, p);
      p += op;
    }
  }
  assert(p === result.length, 'delta result size mismatch');
  return result;
}

type PackEntry = { offset: number; base?: number | string; bytes: Buffer; compressed: Buffer };
// Full entries can be reused verbatim in outgoing packs. Only private, immutable objects are cached.
const compressed = new WeakMap<GitObject, Buffer>();

type Inflated = { buffer: Buffer; engine: { bytesWritten: number } };
type InflateInput = {input:Buffer;limit:number};
const ASYNC_BYTES = 16 * 1024;
function inflateOffThread(input:Buffer,limit:number):Promise<Inflated> {
  return new Promise((resolve,reject)=>inflate(input,{info:true,maxOutputLength:limit},(error,value)=>{
    if(error)reject(error);else resolve(value as unknown as Inflated);
  }));
}
function deflateOffThread(input:Buffer):Promise<Buffer> {
  return new Promise((resolve,reject)=>deflate(input,(error,value)=>error?reject(error):resolve(value)));
}
function *unpackSteps(pack: Buffer, external: Lookup): Generator<InflateInput,Map<string,GitObject>,Inflated> {
  assert(pack.length >= 32 && pack.subarray(0, 4).equals(PACK_MAGIC), 'invalid PACK header');
  const version = pack.readUInt32BE(4);
  const count = pack.readUInt32BE(8);
  assert((version === 2 || version === 3) && count <= MAX_ENTRIES, 'unsupported PACK header');
  const end = pack.length - 20;
  assert(sha1(pack.subarray(0, end)).equals(pack.subarray(end)), 'pack checksum mismatch');
  const src = new Cursor(pack, 12, end);
  const objects = new Map<string, GitObject>();
  const offsets = new Map<number, GitObject>();
  const pending = new Map<number | string, PackEntry[]>();
  const ready: Array<[PackEntry, GitObject]> = [];
  let expanded = 0;
  const charge = (bytes: number) => {
    expanded += bytes;
    assert(expanded <= MAX_EXPANDED_PACK, 'expanded pack too large');
  };
  const notify = (key: number | string, object: GitObject) => {
    const waiters = pending.get(key);
    if (!waiters) return;
    pending.delete(key);
    for (const waiter of waiters) ready.push([waiter, object]);
  };
  const save = (entry: PackEntry, object: GitObject) => {
    const oid = hashObject(object.type, object.data);
    objects.set(oid, object);
    offsets.set(entry.offset, object);
    if (!entry.base) compressed.set(object, entry.compressed);
    notify(oid, object);
    notify(entry.offset, object);
  };
  const drain = () => {
    while (ready.length) {
      const [entry, base] = ready.pop()!;
      const delta = new Cursor(entry.bytes);
      assert(base.data.length === delta.varint(), 'delta base size mismatch');
      const size = delta.varint();
      charge(size);
      save(entry, { type: base.type, data: applyDelta(base.data, entry.bytes, size, delta.p) });
    }
  };
  for (let index = 0; index < count; index++) {
    const offset = src.p;
    const first = src.byte();
    const code = (first >> 4) & 7;
    const size = src.varint(first & 15, 16, Boolean(first & 128));
    const type = TYPES[code];
    let base: number | string | undefined;
    if (code === 6) {
      let digit = src.byte();
      let distance = digit & 127;
      while (digit & 128) {
        digit = src.byte();
        distance = (distance + 1) * 128 + (digit & 127);
        assert(distance <= offset - 12, 'invalid OFS_DELTA base');
      }
      assert(distance > 0 && distance <= offset - 12, 'invalid OFS_DELTA base');
      base = offset - distance;
    } else if (code === 7) {
      base = src.take(20).toString('hex');
    } else {
      assert(type, 'unknown pack entry type');
    }
    charge(size);
    const start = src.p;
    const out = yield {input:pack.subarray(start,end),limit:Math.max(size,1)};
    assert(out.buffer.length === size && out.engine.bytesWritten > 0, 'pack entry size mismatch');
    src.p += out.engine.bytesWritten;
    const entry = { offset, base, bytes: out.buffer, compressed: pack.subarray(start, src.p) };
    if (base === undefined) save(entry, { type: type!, data: out.buffer });
    else {
      const object = typeof base === 'number' ? offsets.get(base) : objects.get(base) ?? external(base);
      if (object) ready.push([entry, object]);
      else {
        const waiters = pending.get(base);
        if (waiters) waiters.push(entry);
        else pending.set(base, [entry]);
      }
    }
    drain();
  }
  assert(src.p === end, 'trailing pack bytes');
  assert(pending.size === 0, 'missing delta base');
  return objects;
}

async function unpack(pack:Buffer,external:Lookup):Promise<Map<string,GitObject>> {
  const steps=unpackSteps(pack,external);
  let step=steps.next();
  while(!step.done){
    const {input,limit}=step.value;
    const result=(pack.length>=ASYNC_BYTES||limit>=ASYNC_BYTES)
      ? await inflateOffThread(input,limit)
      : inflateSync(input,{info:true,maxOutputLength:limit}) as unknown as Inflated;
    step=steps.next(result);
  }
  return step.value;
}
const deflating = new WeakMap<GitObject,Promise<void>>();
function precompress(object:GitObject):Promise<void> {
  const existing=deflating.get(object);
  if(existing)return existing;
  const running=deflateOffThread(object.data).then(bytes=>{compressed.set(object,bytes);}).finally(()=>{deflating.delete(object);});
  deflating.set(object,running);
  return running;
}

function packFor(objects: Iterable<GitObject>, count: number): Buffer {
  const header = Buffer.alloc(12);
  PACK_MAGIC.copy(header);
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(count, 8);
  const chunks: Buffer[] = [header];
  const digest = createHash('sha1').update(header);
  for (const object of objects) {
    let size = object.data.length;
    const sizeBytes = [(TYPES.indexOf(object.type) << 4) | (size & 15)];
    size = Math.floor(size / 16);
    while (size) {
      sizeBytes[sizeBytes.length - 1] |= 128;
      sizeBytes.push(size & 127);
      size = Math.floor(size / 128);
    }
    const entryHeader = Buffer.from(sizeBytes);
    let zlib = compressed.get(object);
    if (!zlib) {
      zlib = deflateSync(object.data);
      compressed.set(object, zlib);
    }
    chunks.push(entryHeader, zlib);
    digest.update(entryHeader).update(zlib);
  }
  chunks.push(digest.digest());
  return Buffer.concat(chunks);
}

function pkt(payload: string | Buffer): Buffer {
  const bytes = typeof payload === 'string' ? Buffer.from(payload) : payload;
  assert(bytes.length + 4 <= MAX_PACKET, 'pkt-line too large');
  return Buffer.concat([Buffer.from((bytes.length + 4).toString(16).padStart(4, '0')), bytes]);
}

function packet(src: Cursor): string | 0 | 1 {
  const prefix = src.take(4).toString('latin1');
  assert(/^[0-9a-fA-F]{4}$/.test(prefix), 'invalid pkt-line prefix');
  const length = Number.parseInt(prefix, 16);
  if (length === 0 || length === 1) return length;
  assert(length >= 4 && length <= MAX_PACKET, 'invalid pkt-line size');
  return src.take(length - 4).toString('utf8').replace(/\n$/, '');
}

function section(src: Cursor, terminator: 0 | 1): string[] {
  const lines: string[] = [];
  for (;;) {
    const line = packet(src);
    if (line === terminator) return lines;
    assert(typeof line === 'string', 'unexpected pkt-line control');
    lines.push(line);
  }
}

function sideband(data: Buffer, limit = MAX_PACKET): Buffer[] {
  const framed: Buffer[] = [];
  const band = Buffer.from([1]);
  for (let p = 0; p < data.length; p += limit - 5) {
    framed.push(pkt(Buffer.concat([band, data.subarray(p, p + limit - 5)])));
  }
  return framed;
}

function validRef(ref: string): boolean {
  return ref.startsWith('refs/') && ref.length <= 1024 && !/\.\.|@\{|[\x00-\x20\x7f~^:?*\[\\]/.test(ref) &&
    !ref.endsWith('.') && ref.split('/').every(part => part && !part.startsWith('.') && !part.endsWith('.lock'));
}

function send(res: ServerResponse, status: number, type: string, data: Buffer): void {
  res.writeHead(status, { 'content-type': type, 'cache-control': 'no-cache', 'content-length': data.length });
  res.end(data);
}

function status(res: ServerResponse, code: number, message: string): void {
  send(res, code, 'application/json', Buffer.from(JSON.stringify({ message })));
}

const V2_AD = Buffer.concat(['version 2\n', 'ls-refs=unborn\n', 'fetch=shallow\n', 'object-format=sha1\n'].map(pkt).concat(FLUSH));

export class GitStore {
  readonly refs = new Map<string, string>();
  readonly #objects = new Map<string, GitObject>();
  // A known-connected object has already had all its links checked. Objects never change in place.
  readonly #connected = new Set<string>();
  readonly #lookup: Lookup = sha => this.#objects.get(sha);

  write(type: GitType, data: Buffer): string {
    assert(TYPES.includes(type) && data.length <= MAX_OBJECT, 'invalid object');
    const sha = hashObject(type, data);
    if (!this.#objects.has(sha)) this.#objects.set(sha, { type, data: Buffer.from(data) });
    return sha;
  }

  read(sha: string): GitObject | undefined {
    const object = this.#lookup(sha);
    return object && { type: object.type, data: Buffer.from(object.data) };
  }

  commit(files: Map<string, GitFile>, parents: string[], message: string): string {
    type Dir = { dirs: Map<string, Dir>; files: Map<string, GitFile> };
    const directory = (): Dir => ({ dirs: new Map(), files: new Map() });
    const root = directory();
    for (const parent of parents) assert(this.#lookup(parent)?.type === 'commit', 'missing commit parent');
    for (const [path, file] of files) {
      const names = path.split('/');
      assert(names.every(name => validName(Buffer.from(name))), 'invalid file path');
      assert(/^(100644|100755|120000|160000)$/.test(file.mode), 'invalid file mode');
      readOid(file.sha);
      assert(file.mode === '160000' || this.#lookup(file.sha)?.type === 'blob', 'missing file blob');
      const leaf = names.pop()!;
      let dir = root;
      for (const name of names) {
        assert(!dir.files.has(name), 'file/directory conflict');
        let node = dir.dirs.get(name);
        if (!node) dir.dirs.set(name, node = directory());
        dir = node;
      }
      assert(!dir.dirs.has(leaf), 'file/directory conflict');
      dir.files.set(leaf, file);
    }
    const encode = (dir: Dir): string => {
      const entries = [...dir.files.entries()];
      for (const [name, sub] of dir.dirs) entries.push([name, { sha: encode(sub), mode: '40000' }]);
      entries.sort(([a, x], [b, y]) => Buffer.compare(Buffer.from(a + (x.mode === '40000' ? '/' : '')), Buffer.from(b + (y.mode === '40000' ? '/' : ''))));
      const chunks: Buffer[] = [];
      for (const [name, file] of entries) chunks.push(Buffer.from(`${file.mode} ${name}\0`), Buffer.from(file.sha, 'hex'));
      return this.write('tree', Buffer.concat(chunks));
    };
    const tree = encode(root);
    const who = 'Forge <forge@localhost> 1700000000 +0000';
    const headers = [`tree ${tree}`, ...parents.map(sha => `parent ${sha}`), `author ${who}`, `committer ${who}`];
    return this.write('commit', Buffer.from(headers.join('\n') + '\n\n' + message + (message.endsWith('\n') ? '' : '\n')));
  }

  files(sha: string): Map<string, GitFile> {
    const result = new Map<string, GitFile>();
    const commit = this.#lookup(sha);
    if (commit?.type !== 'commit') return result;
    const todo = [{ sha: commitParts(commit.data).tree, prefix: '' }];
    while (todo.length) {
      const current = todo.pop()!;
      const tree = this.#lookup(current.sha);
      assert(tree?.type === 'tree', 'missing tree');
      for (const entry of treeEntries(tree.data)) {
        const path = current.prefix + entry.name.toString('utf8');
        if (entry.mode === '40000') todo.push({ sha: entry.sha, prefix: path + '/' });
        else result.set(path, { sha: entry.sha, mode: entry.mode });
      }
    }
    return result;
  }

  history(sha: string): Array<{ sha: string; message: string }> {
    const result: Array<{ sha: string; message: string }> = [];
    const seen = new Set<string>();
    const todo = [sha];
    while (todo.length) {
      const id = todo.pop()!;
      if (seen.has(id)) continue;
      seen.add(id);
      const object = this.#lookup(id);
      if (object?.type !== 'commit') continue;
      const { parents, message } = commitParts(object.data);
      result.push({ sha: id, message: message.replace(/\n$/, '') });
      for (let i = parents.length - 1; i >= 0; i--) todo.push(parents[i]);
    }
    return result;
  }

  isAncestor(ancestor: string, descendant: string): boolean {
    return ancestorOf(ancestor, descendant, this.#lookup);
  }

  #advertise(service: string, v2: boolean): Buffer {
    const upload = service === 'git-upload-pack';
    if (upload && v2) return V2_AD;
    const chunks = [pkt(`# service=${service}\n`), FLUSH];
    const head = this.refs.get(HEAD);
    const capabilities = upload
      ? `side-band-64k ofs-delta shallow no-progress symref=HEAD:${HEAD} object-format=sha1`
      : 'report-status side-band-64k delete-refs ofs-delta object-format=sha1';
    const refs = [...this.refs].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0);
    if (upload && head) refs.unshift(['HEAD', head]);
    if (!refs.length && !upload) refs.push(['capabilities^{}', ZERO]);
    for (const [index, [name, sha]] of refs.entries()) {
      chunks.push(pkt(`${sha} ${name}${index === 0 ? '\0' + capabilities : ''}\n`));
      if (upload && name.startsWith('refs/tags/')) {
        const target = peel(sha, this.#lookup);
        if (target) chunks.push(pkt(`${target} ${name}^{}\n`));
      }
    }
    return Buffer.concat([...chunks, FLUSH]);
  }

  #lsRefs(args: string[]): Buffer {
    const chunks: Buffer[] = [];
    const prefixes = args.filter(line => line.startsWith('ref-prefix ')).map(line => line.slice(11));
    const accept = (name: string) => !prefixes.length || prefixes.some(prefix => name.startsWith(prefix));
    const head = this.refs.get(HEAD);
    if (accept('HEAD')) {
      if (head) chunks.push(pkt(`${head} HEAD${args.includes('symrefs') ? ' symref-target:' + HEAD : ''}\n`));
      else if (args.includes('unborn')) chunks.push(pkt(`unborn HEAD symref-target:${HEAD}\n`));
    }
    for (const [name, sha] of this.refs) {
      if (!accept(name)) continue;
      const target = args.includes('peel') ? peel(sha, this.#lookup) : undefined;
      chunks.push(pkt(`${sha} ${name}${target ? ' peeled:' + target : ''}\n`));
    }
    return Buffer.concat([...chunks, FLUSH]);
  }

  #uploadInFlight = 0;
  async #fetch(args: string[], v2: boolean): Promise<Buffer> {
    const wants = new Set<string>();
    const haves = new Set<string>();
    const shallow = new Set<string>();
    const features = new Set<string>();
    let depth = 0;
    let done = false;
    for (const line of args) {
      const [key, value, ...rest] = line.split(' ');
      if (key === 'want') {
        wants.add(readOid(value));
        if (!v2) for (const feature of rest) features.add(feature);
      } else if (key === 'have') {
        const sha = readOid(value);
        if (this.#lookup(sha)?.type === 'commit') haves.add(sha);
      } else if (key === 'shallow') {
        const sha = readOid(value);
        if (this.#lookup(sha)?.type === 'commit') shallow.add(sha);
      } else if (key === 'deepen') {
        depth = Number(value);
        assert(/^\d+$/.test(value) && Number.isSafeInteger(depth) && depth > 0, 'invalid depth');
      } else if (key === 'done') {
        done = true;
      } else if (['thin-pack', 'no-progress', 'ofs-delta', 'include-tag'].includes(key)) {
        features.add(key);
      } else {
        throw new Error('unsupported fetch argument');
      }
    }
    if (!wants.size) return FLUSH;
    const lookup = this.#lookup;
    const sendable = new Set(this.refs.values());
    for (const tip of this.refs.values()) {
      const peeled = peel(tip, lookup);
      if (peeled) sendable.add(peeled);
    }
    // Permit historical wants in this repository, but never expose unreferenced/quarantined objects.
    let reachable: Set<string> | undefined;
    for (const want of wants) {
      if (sendable.has(want)) continue;
      reachable ??= walk(this.refs.values(), lookup);
      assert(reachable.has(want), 'want is not reachable');
    }
    const chunks: Buffer[] = [];
    if (!done && v2) {
      chunks.push(pkt('acknowledgments\n'));
      if (haves.size) for (const sha of haves) chunks.push(pkt(`ACK ${sha}\n`));
      else chunks.push(pkt('NAK\n'));
      return Buffer.concat([...chunks, FLUSH]);
    }
    const boundary = new Set(shallow);
    const shallowInfo: Buffer[] = [];
    if (depth) {
      const distances = new Map<string, number>();
      const todo: Array<[string, number]> = [];
      for (const sha of wants) todo.push([peel(sha, lookup) ?? sha, 1]);
      const candidates = new Map<string, string[]>();
      for (let index = 0; index < todo.length; index++) {
        const [sha, distance] = todo[index];
        if (distances.has(sha)) continue;
        const object = lookup(sha);
        if (object?.type !== 'commit') continue;
        distances.set(sha, distance);
        const { parents } = commitParts(object.data);
        if (distance < depth) for (const parent of parents) todo.push([parent, distance + 1]);
        else if (parents.length) candidates.set(sha, parents);
      }
      for (const [sha, parents] of candidates) {
        if (!parents.some(parent => !distances.has(parent))) continue;
        boundary.add(sha);
        if (!shallow.has(sha)) shallowInfo.push(pkt(`shallow ${sha}\n`));
      }
      for (const sha of shallow) {
        if (!distances.has(sha)) continue;
        const parents = commitParts(lookup(sha)!.data).parents;
        if (parents.every(parent => distances.has(parent))) {
          boundary.delete(sha);
          shallowInfo.push(pkt(`unshallow ${sha}\n`));
        }
      }
    }
    if (v2) {
      if (shallowInfo.length) chunks.push(pkt('shallow-info\n'), ...shallowInfo, DELIM);
      chunks.push(pkt('packfile\n'));
    } else {
      if (depth) {
        for (const line of shallowInfo) chunks.push(line);
        chunks.push(FLUSH);
      }
      const common = [...haves].at(-1);
      chunks.push(pkt(common ? `ACK ${common}\n` : 'NAK\n'));
      if (!done) return Buffer.concat(chunks);
    }
    const held = walk(haves, lookup, shallow);
    // For a deepen, traversal must still cross held commits to reach their newly visible parents.
    const selection = walk(wants, lookup, boundary, depth ? undefined : held);
    for (const sha of held) selection.delete(sha);
    if (features.has('include-tag')) {
      for (const [ref, sha] of this.refs) {
        if (!ref.startsWith('refs/tags/')) continue;
        const target = peel(sha, lookup);
        if (target && selection.has(target)) {
          let current = sha;
          for (let object = lookup(current); object?.type === 'tag'; object = lookup(current)) {
            if (!held.has(current)) selection.add(current);
            current = tagTarget(object.data).sha;
          }
        }
      }
    }
    const objects=Array.from(selection,sha=>lookup(sha)!);
    const uncompressed=objects.filter(object=>!compressed.has(object));
    if(uncompressed.length && (this.#uploadInFlight>1 || uncompressed.reduce((n,o)=>n+o.data.length,0)>=ASYNC_BYTES)){
      // Bound outstanding zlib jobs per request; build the pack in traversal order afterward.
      for(let i=0;i<uncompressed.length;i+=4)await Promise.all(uncompressed.slice(i,i+4).map(precompress));
    }
    const pack = packFor(objects, selection.size);
    if (v2 || features.has('side-band-64k') || features.has('side-band')) {
      for (const frame of sideband(pack, !v2 && !features.has('side-band-64k') ? 1000 : MAX_PACKET)) chunks.push(frame);
      chunks.push(FLUSH);
    } else chunks.push(pack);
    return Buffer.concat(chunks);
  }

  async #upload(body: Buffer, v2: boolean): Promise<Buffer> {
    const src = new Cursor(body);
    if (!v2) {
      const wants = section(src, 0);
      if (!wants.length) return FLUSH;
      const negotiation: string[] = [];
      while (src.p < src.end) {
        const item = packet(src);
        if (item === 0) break;
        assert(typeof item === 'string', 'invalid negotiation');
        negotiation.push(item);
      }
      assert(src.p === src.end, 'trailing request bytes');
      return this.#fetch(wants.concat(negotiation), false);
    }
    const capabilities = section(src, 1);
    const args = section(src, 0);
    assert(src.p === src.end, 'trailing request bytes');
    assert(!capabilities.some(line => line.startsWith('object-format=') && line !== 'object-format=sha1'), 'unsupported object format');
    if (capabilities[0] === 'command=ls-refs') return this.#lsRefs(args);
    if (capabilities[0] === 'command=fetch') return this.#fetch(args, true);
    throw new Error('unsupported upload-pack command');
  }

  async #receive(body: Buffer): Promise<Buffer> {
    const src = new Cursor(body);
    const raw = section(src, 0);
    if (!raw.length) return Buffer.alloc(0);
    const [first, cap = '', ...extra] = raw[0].split('\0');
    assert(!extra.length, 'invalid receive-pack capabilities');
    raw[0] = first;
    const capabilities = new Set(cap.split(' '));
    const commands = raw.map(line => {
      const [old, next, ref, ...rest] = line.split(' ');
      assert(old && next && ref && !rest.length && OID.test(old) && OID.test(next) && !/[\x00-\x20\x7f]/.test(ref), 'invalid receive-pack command');
      return { old, next, ref };
    });
    let incoming = new Map<string, GitObject>();
    let unpackError: string | undefined;
    try {
      if (src.p < src.end) incoming = await unpack(body.subarray(src.p), this.#lookup);
      else assert(commands.every(cmd => cmd.next === ZERO), 'missing pack');
    } catch {
      unpackError = 'invalid pack';
    }
    const lookup: Lookup = sha => this.#lookup(sha) ?? incoming.get(sha);
    const report = [pkt(`unpack ${unpackError ?? 'ok'}\n`)];
    const seen = new Set<string>();
    for (const { old, next, ref } of commands) {
      let error = unpackError ? 'unpacker error' : '';
      if (!error && !validRef(ref)) error = 'invalid ref name';
      if (!error && seen.has(ref)) error = 'duplicate command';
      seen.add(ref);
      if (!error && old !== (this.refs.get(ref) ?? ZERO)) error = 'stale ref';
      if (!error && next === ZERO && old === ZERO) error = 'no such ref';
      if (!error && next !== ZERO) {
        if (ref.startsWith('refs/heads/') && lookup(next)?.type !== 'commit') error = 'branch tip must be a commit';
        if (!error && [...this.refs.keys()].some(existing => ref.startsWith(existing + '/') || existing.startsWith(ref + '/'))) error = 'ref namespace conflict';
        if (!error) {
          try {
            const closure = walk([next], lookup, undefined, this.#connected);
            if (old !== ZERO && old !== next && !ancestorOf(old, next, lookup)) error = ref.startsWith('refs/tags/') ? 'tag already exists' : 'non-fast-forward';
            if (!error) {
              for (const sha of closure) {
                const object = incoming.get(sha);
                if (object && !this.#objects.has(sha)) {
                  // Do not pin the entire request (including rejected objects) via a zlib slice.
                  const entry = compressed.get(object);
                  if (entry) compressed.set(object, Buffer.from(entry));
                  this.#objects.set(sha, object);
                }
                this.#connected.add(sha);
              }
              this.refs.set(ref, next);
            }
          } catch {
            error = 'invalid object graph';
          }
        }
      } else if (!error) this.refs.delete(ref);
      report.push(pkt(error ? `ng ${ref} ${error}\n` : `ok ${ref}\n`));
    }
    const hasReport = capabilities.has('report-status') || capabilities.has('report-status-v2');
    if (!hasReport) return capabilities.has('side-band-64k') ? FLUSH : Buffer.alloc(0);
    const bytes = Buffer.concat([...report, FLUSH]);
    return capabilities.has('side-band-64k') ? Buffer.concat([...sideband(bytes), FLUSH]) : bytes;
  }

  // The application authenticates and resolves the repository before handing the request here.
  async http(req: IncomingMessage, res: ServerResponse, url: URL, service: string, body: Buffer): Promise<void> {
    const v2 = /(?:^|:)\s*version=2(?:\s*:|\s*$)/.test(String(req.headers['git-protocol'] ?? ''));
    try {
      if (service === 'info/refs') {
        if (req.method !== 'GET') return status(res, 405, 'Method Not Allowed');
        const target = url.searchParams.get('service');
        if (target !== 'git-upload-pack' && target !== 'git-receive-pack') return status(res, 403, 'Unsupported Git service');
        return send(res, 200, `application/x-${target}-advertisement`, this.#advertise(target, v2));
      }
      if (service !== 'git-upload-pack' && service !== 'git-receive-pack') return status(res, 404, 'Not Found');
      if (req.method !== 'POST') return status(res, 405, 'Method Not Allowed');
      if (body.length > MAX_EXPANDED_PACK) return status(res, 413, 'Git request too large');
      let result:Buffer;
      if(service==='git-upload-pack'){
        this.#uploadInFlight++;
        try{result=await this.#upload(body,v2);}finally{this.#uploadInFlight--;}
      } else result=await this.#receive(body);
      if(!res.destroyed)send(res, 200, `application/x-${service}-result`, result);
    } catch {
      status(res, 400, 'Malformed Git request');
    }
  }
}
