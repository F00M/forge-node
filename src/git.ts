// Git object model: the content-addressed object store, commit/tree/tag parsing, tree building,
// packfile reading (with OFS/REF delta resolution) and writing, and pkt-line framing.
// Everything is synchronous: a request is answered in one turn of the event loop.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0000000000000000000000000000000000000000';

export const OBJ_COMMIT = 1;
export const OBJ_TREE = 2;
export const OBJ_BLOB = 3;
export const OBJ_TAG = 4;
const OBJ_OFS_DELTA = 6;
const OBJ_REF_DELTA = 7;

export const TYPE_NAMES = ['', 'commit', 'tree', 'blob', 'tag'];

export interface Signature {
  name: string;
  email: string;
  /** Seconds since the epoch. */
  time: number;
  /** The raw timezone, e.g. "+0000". */
  tz: string;
}

export interface Commit {
  tree: string;
  parents: string[];
  author: Signature;
  committer: Signature;
  message: string;
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export interface Tag {
  object: string;
  type: string;
  tag: string;
  message: string;
}

export interface GitObject {
  type: number;
  data: Buffer;
  /** Pack-ready bytes: the object header plus the deflated data, built once on first send. */
  packed?: Buffer;
  commit?: Commit;
  tree?: TreeEntry[];
  tag?: Tag;
}

export class GitError extends Error {}

// ------------------------------------------------------------------ hashing and the store

export function hashObject(type: number, data: Buffer): string {
  return createHash('sha1').update(`${TYPE_NAMES[type]} ${data.length}\0`, 'latin1').update(data).digest('hex');
}

/** One object store shared by every repository (objects are content-addressed, like a fork network). */
export class ObjectStore {
  readonly objects = new Map<string, GitObject>();

  has(oid: string): boolean {
    return this.objects.has(oid);
  }

  get(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  /** Store an object whose id is already known (pack ingest verified it), keeping an existing copy. */
  putKnown(oid: string, type: number, data: Buffer): void {
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
  }

  put(type: number, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
    return oid;
  }

  commit(oid: string): Commit | null {
    const obj = this.objects.get(oid);
    if (obj === undefined || obj.type !== OBJ_COMMIT) return null;
    if (obj.commit === undefined) obj.commit = parseCommit(obj.data);
    return obj.commit;
  }

  tree(oid: string): TreeEntry[] | null {
    const obj = this.objects.get(oid);
    if (obj === undefined || obj.type !== OBJ_TREE) return null;
    if (obj.tree === undefined) obj.tree = parseTree(obj.data);
    return obj.tree;
  }

  tag(oid: string): Tag | null {
    const obj = this.objects.get(oid);
    if (obj === undefined || obj.type !== OBJ_TAG) return null;
    if (obj.tag === undefined) obj.tag = parseTag(obj.data);
    return obj.tag;
  }

  /** Follow annotated tags down to the first non-tag object. */
  peel(oid: string): string {
    let current = oid;
    for (let i = 0; i < 16; i += 1) {
      const tag = this.tag(current);
      if (tag === null) return current;
      current = tag.object;
    }
    return current;
  }

  /** The pack-ready bytes of one stored object. */
  packed(oid: string): Buffer {
    const obj = this.objects.get(oid);
    if (obj === undefined) throw new GitError(`missing object ${oid}`);
    if (obj.packed === undefined) obj.packed = Buffer.concat([packObjectHeader(obj.type, obj.data.length), deflateSync(obj.data, { level: 1 })]);
    return obj.packed;
  }
}

// ------------------------------------------------------------------ parsing

function parseSignature(text: string): Signature {
  const lt = text.indexOf('<');
  const gt = text.indexOf('>', lt + 1);
  if (lt < 0 || gt < 0) return { name: text.trim(), email: '', time: 0, tz: '+0000' };
  const name = text.slice(0, lt).trim();
  const email = text.slice(lt + 1, gt);
  const rest = text.slice(gt + 1).trim().split(' ');
  const time = Number.parseInt(rest[0] ?? '0', 10);
  return { name, email, time: Number.isFinite(time) ? time : 0, tz: rest[1] ?? '+0000' };
}

export function parseCommit(data: Buffer): Commit {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author: Signature = { name: '', email: '', time: 0, tz: '+0000' };
  let committer: Signature = author;
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5, 45);
    else if (line.startsWith('parent ')) parents.push(line.slice(7, 47));
    else if (line.startsWith('author ')) author = parseSignature(line.slice(7));
    else if (line.startsWith('committer ')) committer = parseSignature(line.slice(10));
  }
  return { tree, parents, author, committer, message };
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let p = 0;
  while (p < data.length) {
    const space = data.indexOf(0x20, p);
    if (space < 0) throw new GitError('corrupt tree: no mode terminator');
    const nul = data.indexOf(0, space + 1);
    if (nul < 0 || nul + 21 > data.length) throw new GitError('corrupt tree: truncated entry');
    const mode = data.toString('latin1', p, space);
    const name = data.toString('utf8', space + 1, nul);
    const oid = data.toString('hex', nul + 1, nul + 21);
    entries.push({ mode, name, oid });
    p = nul + 21;
  }
  return entries;
}

export function parseTag(data: Buffer): Tag {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  let object = '';
  let type = '';
  let tag = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7, 47);
    else if (line.startsWith('type ')) type = line.slice(5);
    else if (line.startsWith('tag ')) tag = line.slice(4);
  }
  return { object, type, tag, message: split < 0 ? '' : text.slice(split + 2) };
}

// ------------------------------------------------------------------ building objects

function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

export function serializeTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = Buffer.from(treeSortKey(a), 'utf8');
    const kb = Buffer.from(treeSortKey(b), 'utf8');
    return Buffer.compare(ka, kb);
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

function formatSignature(sig: Signature): string {
  return `${sig.name} <${sig.email}> ${sig.time} ${sig.tz}`;
}

export function serializeCommit(commit: Commit): Buffer {
  let text = `tree ${commit.tree}\n`;
  for (const parent of commit.parents) text += `parent ${parent}\n`;
  text += `author ${formatSignature(commit.author)}\ncommitter ${formatSignature(commit.committer)}\n\n${commit.message}`;
  return Buffer.from(text, 'utf8');
}

/** A flattened view of a tree: path -> {mode, oid} for every blob (and gitlink) below it. */
export function flattenTree(store: ObjectStore, treeOid: string, prefix = '', out = new Map<string, { mode: string; oid: string }>()): Map<string, { mode: string; oid: string }> {
  const entries = store.tree(treeOid);
  if (entries === null) return out;
  for (const entry of entries) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') flattenTree(store, entry.oid, path, out);
    else out.set(path, { mode: entry.mode, oid: entry.oid });
  }
  return out;
}

/** Build (and store) a tree hierarchy from a flat path map; returns the root tree id. */
export function buildTree(store: ObjectStore, files: Map<string, { mode: string; oid: string }>): string {
  interface Dir {
    files: TreeEntry[];
    dirs: Map<string, Dir>;
  }
  const root: Dir = { files: [], dirs: new Map() };
  for (const [path, entry] of files) {
    const parts = path.split('/');
    let dir = root;
    for (let i = 0; i < parts.length - 1; i += 1) {
      const name = parts[i]!;
      let next = dir.dirs.get(name);
      if (next === undefined) {
        next = { files: [], dirs: new Map() };
        dir.dirs.set(name, next);
      }
      dir = next;
    }
    dir.files.push({ mode: entry.mode, name: parts[parts.length - 1]!, oid: entry.oid });
  }
  const write = (dir: Dir): string => {
    const entries: TreeEntry[] = [...dir.files];
    for (const [name, child] of dir.dirs) entries.push({ mode: '40000', name, oid: write(child) });
    return store.put(OBJ_TREE, serializeTree(entries));
  };
  return write(root);
}

/** Find the entry at `path` below a root tree: a blob/gitlink entry, a tree entry, or null. */
export function lookupPath(store: ObjectStore, treeOid: string, path: string): TreeEntry | null {
  const parts = path.split('/').filter((part) => part !== '');
  if (parts.length === 0) return { mode: '40000', name: '', oid: treeOid };
  let current = treeOid;
  for (let i = 0; i < parts.length; i += 1) {
    const entries = store.tree(current);
    if (entries === null) return null;
    const found = entries.find((entry) => entry.name === parts[i]);
    if (found === undefined) return null;
    if (i === parts.length - 1) return found;
    if (found.mode !== '40000') return null;
    current = found.oid;
  }
  return null;
}

// ------------------------------------------------------------------ history walks

/** Whether `ancestor` is reachable from `descendant` (inclusive). */
export function isAncestor(store: ObjectStore, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const commit = store.commit(oid);
    if (commit === null) continue;
    for (const parent of commit.parents) if (!seen.has(parent)) stack.push(parent);
  }
  return false;
}

/** Every commit reachable from `start` (inclusive). */
export function ancestry(store: ObjectStore, start: string): Set<string> {
  const seen = new Set<string>();
  const stack = [start];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (seen.has(oid)) continue;
    const commit = store.commit(oid);
    if (commit === null) continue;
    seen.add(oid);
    for (const parent of commit.parents) if (!seen.has(parent)) stack.push(parent);
  }
  return seen;
}

/** The best common ancestor of two commits (nearest to `b` by breadth-first order), or null. */
export function mergeBase(store: ObjectStore, a: string, b: string): string | null {
  const fromA = ancestry(store, a);
  if (fromA.has(b)) return b;
  const seen = new Set<string>();
  const queue = [b];
  for (let i = 0; i < queue.length; i += 1) {
    const oid = queue[i]!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    if (fromA.has(oid)) return oid;
    const commit = store.commit(oid);
    if (commit === null) continue;
    for (const parent of commit.parents) queue.push(parent);
  }
  return null;
}

/** Commits reachable from `start`, newest first (committer time, then discovery order), up to `limit`. */
export function logFrom(store: ObjectStore, start: string, limit: number, exclude?: Set<string>): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const heap: string[] = [start];
  while (heap.length > 0 && out.length < limit) {
    let best = 0;
    for (let i = 1; i < heap.length; i += 1) {
      const ci = store.commit(heap[i]!);
      const cb = store.commit(heap[best]!);
      if ((ci?.committer.time ?? 0) > (cb?.committer.time ?? 0)) best = i;
    }
    const oid = heap.splice(best, 1)[0]!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    if (exclude !== undefined && exclude.has(oid)) continue;
    const commit = store.commit(oid);
    if (commit === null) continue;
    out.push(oid);
    for (const parent of commit.parents) if (!seen.has(parent)) heap.push(parent);
  }
  return out;
}

// ------------------------------------------------------------------ packfiles

function packObjectHeader(type: number, size: number): Buffer {
  const bytes: number[] = [];
  let c = (type << 4) | (size & 0x0f);
  let rest = Math.floor(size / 16);
  while (rest > 0) {
    bytes.push(c | 0x80);
    c = rest & 0x7f;
    rest = Math.floor(rest / 128);
  }
  bytes.push(c);
  return Buffer.from(bytes);
}

/** A version-2 pack of the given stored objects, undeltified, with its SHA-1 trailer. */
export function writePack(store: ObjectStore, oids: Iterable<string>): Buffer {
  const parts: Buffer[] = [];
  let count = 0;
  for (const oid of oids) {
    parts.push(store.packed(oid));
    count += 1;
  }
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'latin1');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(count, 8);
  parts.unshift(header);
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

function readDeltaSize(delta: Buffer, start: number): [number, number] {
  let p = start;
  let size = 0;
  let shift = 1;
  for (;;) {
    if (p >= delta.length) throw new GitError('corrupt delta header');
    const c = delta[p++]!;
    size += (c & 0x7f) * shift;
    shift *= 128;
    if ((c & 0x80) === 0) break;
  }
  return [size, p];
}

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let [baseSize, p] = readDeltaSize(delta, 0);
  if (baseSize !== base.length) throw new GitError('delta base size mismatch');
  const [resultSize, q] = readDeltaSize(delta, p);
  p = q;
  const out = Buffer.allocUnsafe(resultSize);
  let o = 0;
  while (p < delta.length) {
    const op = delta[p++]!;
    if (op & 0x80) {
      let offset = 0;
      let length = 0;
      if (op & 0x01) offset = delta[p++]!;
      if (op & 0x02) offset |= delta[p++]! << 8;
      if (op & 0x04) offset |= delta[p++]! << 16;
      if (op & 0x08) offset += delta[p++]! * 0x1000000;
      if (op & 0x10) length = delta[p++]!;
      if (op & 0x20) length |= delta[p++]! << 8;
      if (op & 0x40) length |= delta[p++]! << 16;
      if (length === 0) length = 0x10000;
      if (offset + length > base.length || o + length > resultSize) throw new GitError('delta copy out of range');
      base.copy(out, o, offset, offset + length);
      o += length;
    } else if (op !== 0) {
      if (p + op > delta.length || o + op > resultSize) throw new GitError('delta insert out of range');
      delta.copy(out, o, p, p + op);
      o += op;
      p += op;
    } else {
      throw new GitError('delta opcode 0');
    }
  }
  if (o !== resultSize) throw new GitError('delta result size mismatch');
  return out;
}

export interface IngestedObject {
  oid: string;
  type: number;
  data: Buffer;
}

/**
 * Parse a pack and resolve every delta. REF_DELTA bases may live in the pack (before or after the delta)
 * or in the store (a thin pack). Returns the objects in pack order; nothing is stored here.
 */
export function readPack(store: ObjectStore, pack: Buffer): IngestedObject[] {
  if (pack.length < 32 || pack.toString('latin1', 0, 4) !== 'PACK') throw new GitError('not a pack');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new GitError(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const end = pack.length - 20;
  const expected = pack.toString('hex', end);
  const actual = createHash('sha1').update(pack.subarray(0, end)).digest('hex');
  if (expected !== actual) throw new GitError('pack checksum mismatch');

  interface Raw {
    offset: number;
    type: number;
    data: Buffer;
    baseOffset?: number;
    baseOid?: string;
    resolved?: IngestedObject;
  }
  const raws: Raw[] = [];
  const byOffset = new Map<number, Raw>();
  let p = 12;
  for (let i = 0; i < count; i += 1) {
    if (p >= end) throw new GitError('pack truncated');
    const offset = p;
    let c = pack[p++]!;
    const type = (c >> 4) & 7;
    let size = c & 0x0f;
    let shift = 16;
    while (c & 0x80) {
      c = pack[p++]!;
      size += (c & 0x7f) * shift;
      shift *= 128;
    }
    const raw: Raw = { offset, type, data: Buffer.alloc(0) };
    if (type === OBJ_OFS_DELTA) {
      c = pack[p++]!;
      let back = c & 0x7f;
      while (c & 0x80) {
        c = pack[p++]!;
        back = (back + 1) * 128 + (c & 0x7f);
      }
      raw.baseOffset = offset - back;
    } else if (type === OBJ_REF_DELTA) {
      raw.baseOid = pack.toString('hex', p, p + 20);
      p += 20;
    } else if (type < 1 || type > 4) {
      throw new GitError(`bad pack object type ${type}`);
    }
    const inflated = inflateSync(pack.subarray(p, end), { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    raw.data = inflated.buffer;
    if (raw.data.length !== size) throw new GitError('pack object size mismatch');
    p += inflated.engine.bytesWritten;
    raws.push(raw);
    byOffset.set(offset, raw);
  }

  const byOid = new Map<string, IngestedObject>();
  const resolve = (raw: Raw): IngestedObject | null => {
    if (raw.resolved !== undefined) return raw.resolved;
    let baseType: number;
    let baseData: Buffer;
    if (raw.type === OBJ_OFS_DELTA) {
      const base = byOffset.get(raw.baseOffset!);
      if (base === undefined) throw new GitError('ofs-delta base not found');
      const resolvedBase = resolve(base);
      if (resolvedBase === null) return null;
      baseType = resolvedBase.type;
      baseData = resolvedBase.data;
    } else if (raw.type === OBJ_REF_DELTA) {
      const inPack = byOid.get(raw.baseOid!);
      if (inPack !== undefined) {
        baseType = inPack.type;
        baseData = inPack.data;
      } else {
        const stored = store.get(raw.baseOid!);
        if (stored === undefined) return null;
        baseType = stored.type;
        baseData = stored.data;
      }
    } else {
      const object = { oid: hashObject(raw.type, raw.data), type: raw.type, data: raw.data };
      raw.resolved = object;
      byOid.set(object.oid, object);
      return object;
    }
    const data = applyDelta(baseData, raw.data);
    const object = { oid: hashObject(baseType, data), type: baseType, data };
    raw.resolved = object;
    byOid.set(object.oid, object);
    return object;
  };
  let pending = raws.filter((raw) => resolve(raw) === null);
  while (pending.length > 0) {
    const next = pending.filter((raw) => resolve(raw) === null);
    if (next.length === pending.length) throw new GitError(`unresolved delta base ${next[0]!.baseOid ?? '?'}`);
    pending = next;
  }
  return raws.map((raw) => raw.resolved!);
}

// ------------------------------------------------------------------ pkt-lines

const HEX = '0123456789abcdef';

function pktHeader(length: number): string {
  return HEX[(length >> 12) & 15]! + HEX[(length >> 8) & 15]! + HEX[(length >> 4) & 15]! + HEX[length & 15]!;
}

/** One pkt-line holding `text` (UTF-8). */
export function pkt(text: string): string {
  return pktHeader(Buffer.byteLength(text) + 4) + text;
}

export function pktBuffer(data: Buffer): Buffer {
  return Buffer.concat([Buffer.from(pktHeader(data.length + 4), 'latin1'), data]);
}

export const FLUSH = '0000';
export const DELIM = '0001';

/** Band-1 sideband framing of `data` in pkt-lines of at most 65520 bytes. */
export function sideband(data: Buffer, band = 1): Buffer[] {
  const out: Buffer[] = [];
  const max = 65515;
  for (let i = 0; i < data.length; i += max) {
    const chunk = data.subarray(i, Math.min(data.length, i + max));
    const head = Buffer.allocUnsafe(5);
    head.write(pktHeader(chunk.length + 5), 0, 'latin1');
    head[4] = band;
    out.push(head, chunk);
  }
  return out;
}

export type Packet = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'end' };

/** Split a request body into pkt-lines; `rest` is the offset after the last parsed packet. */
export function readPackets(body: Buffer, start = 0, stopAfterFlush = false): { packets: Packet[]; rest: number } {
  const packets: Packet[] = [];
  let p = start;
  while (p + 4 <= body.length) {
    const length = Number.parseInt(body.toString('latin1', p, p + 4), 16);
    if (!Number.isFinite(length)) throw new GitError('bad pkt-line length');
    if (length === 0) {
      packets.push({ kind: 'flush' });
      p += 4;
      if (stopAfterFlush) break;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim' });
      p += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'end' });
      p += 4;
      continue;
    }
    if (length < 4 || p + length > body.length) throw new GitError('truncated pkt-line');
    packets.push({ kind: 'data', payload: body.subarray(p + 4, p + length) });
    p += length;
  }
  return { packets, rest: p };
}

/** A data packet's text without its trailing newline. */
export function packetText(packet: Packet): string {
  if (packet.kind !== 'data') return '';
  const text = packet.payload.toString('utf8');
  return text.endsWith('\n') ? text.slice(0, -1) : text;
}
