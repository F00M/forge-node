// Git smart HTTP: info/refs advertisements, upload-pack (protocol v2 ls-refs and fetch, plus v0 fetch)
// and receive-pack (v0, report-status over side-band-64k). Pack answers are cached per repository
// version, so repeated identical fetches (a clone storm) cost one map lookup each.

import {
  DELIM,
  FLUSH,
  GitError,
  OBJ_COMMIT,
  OBJ_TAG,
  OBJ_TREE,
  ZERO_OID,
  isAncestor,
  packetText,
  pkt,
  pktBuffer,
  readPack,
  readPackets,
  sideband,
  writePack,
  type Packet,
} from './git.ts';
import type { Forge, Repo } from './forge.ts';
import type { Response } from './http.ts';

const AGENT = 'git/forge-node-1.0';
const UPLOAD_ADVERT = 'application/x-git-upload-pack-advertisement';
const RECEIVE_ADVERT = 'application/x-git-receive-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';
const NO_CACHE = 'Cache-Control: no-cache, max-age=0, must-revalidate\r\nExpires: Fri, 01 Jan 1980 00:00:00 GMT\r\nPragma: no-cache\r\n';

const V2_CAPABILITIES = `${pkt('version 2\n')}${pkt(`agent=${AGENT}\n`)}${pkt('ls-refs=unborn\n')}${pkt('fetch=shallow wait-for-done\n')}${pkt('server-option\n')}${pkt('object-format=sha1\n')}${FLUSH}`;

interface CacheEntry {
  version: number;
  body: Buffer;
}

/** Sorted ref names (git orders refs bytewise). */
function sortedRefs(repo: Repo): Array<[string, string]> {
  return [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
}

export class SmartHttp {
  private readonly forge: Forge;
  private readonly packCache = new Map<string, CacheEntry>();

  constructor(forge: Forge) {
    this.forge = forge;
  }

  // ---------------------------------------------------------------- advertisements

  infoRefs(repo: Repo, service: string, gitProtocol: string): Response {
    if (service === 'git-upload-pack') {
      if (gitProtocol.includes('version=2')) {
        return { status: 200, type: UPLOAD_ADVERT, body: V2_CAPABILITIES, extra: NO_CACHE };
      }
      return { status: 200, type: UPLOAD_ADVERT, body: this.v0Advertisement(repo, 'upload'), extra: NO_CACHE };
    }
    return { status: 200, type: RECEIVE_ADVERT, body: this.v0Advertisement(repo, 'receive'), extra: NO_CACHE };
  }

  private v0Advertisement(repo: Repo, kind: 'upload' | 'receive'): string {
    const head = kind === 'upload' ? pkt('# service=git-upload-pack\n') + FLUSH : pkt('# service=git-receive-pack\n') + FLUSH;
    const symref = `symref=HEAD:refs/heads/${repo.defaultBranch}`;
    const caps =
      kind === 'upload'
        ? `multi_ack_detailed no-done side-band-64k side-band thin-pack ofs-delta shallow no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want ${symref} object-format=sha1 agent=${AGENT}`
        : `report-status delete-refs side-band-64k quiet ofs-delta object-format=sha1 agent=${AGENT}`;
    const lines: string[] = [];
    const refs = sortedRefs(repo);
    if (kind === 'upload') {
      const headOid = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      if (headOid !== undefined) lines.push(`${headOid} HEAD`);
    }
    for (const [name, oid] of refs) {
      lines.push(`${oid} ${name}`);
      if (kind === 'upload' && name.startsWith('refs/tags/')) {
        const peeled = this.forge.store.peel(oid);
        if (peeled !== oid) lines.push(`${peeled} ${name}^{}`);
      }
    }
    let body = head;
    if (lines.length === 0) body += pkt(`${ZERO_OID} capabilities^{}\0${caps}\n`);
    else {
      body += pkt(`${lines[0]}\0${caps}\n`);
      for (let i = 1; i < lines.length; i += 1) body += pkt(`${lines[i]}\n`);
    }
    return body + FLUSH;
  }

  // ---------------------------------------------------------------- upload-pack

  uploadPack(repo: Repo, body: Buffer, gitProtocol: string): Response {
    if (gitProtocol.includes('version=2')) return this.uploadPackV2(repo, body);
    return this.uploadPackV0(repo, body);
  }

  private uploadPackV2(repo: Repo, body: Buffer): Response {
    const cacheKey = `${repo.id}\0${body.toString('latin1')}`;
    const cached = this.packCache.get(cacheKey);
    if (cached !== undefined && cached.version === repo.version) return { status: 200, type: UPLOAD_RESULT, body: cached.body, extra: NO_CACHE };
    const { packets } = readPackets(body);
    let command = '';
    let i = 0;
    for (; i < packets.length; i += 1) {
      const packet = packets[i]!;
      if (packet.kind !== 'data') break;
      const text = packetText(packet);
      if (text.startsWith('command=')) command = text.slice(8);
    }
    if (packets[i]?.kind === 'delim') i += 1;
    const args: string[] = [];
    for (; i < packets.length; i += 1) {
      const packet = packets[i]!;
      if (packet.kind !== 'data') break;
      args.push(packetText(packet));
    }
    let out: Buffer;
    if (command === 'ls-refs') out = Buffer.from(this.lsRefs(repo, args), 'utf8');
    else if (command === 'fetch') out = this.fetchV2(repo, args);
    else if (command === '' && packets.length === 0) out = Buffer.from(FLUSH, 'latin1');
    else return { status: 400, type: 'text/plain; charset=utf-8', body: `unknown command ${command}\n` };
    if (this.packCache.size > 256) this.packCache.clear();
    this.packCache.set(cacheKey, { version: repo.version, body: out });
    return { status: 200, type: UPLOAD_RESULT, body: out, extra: NO_CACHE };
  }

  private lsRefs(repo: Repo, args: string[]): string {
    let peel = false;
    let symrefs = false;
    let unborn = false;
    const prefixes: string[] = [];
    for (const arg of args) {
      if (arg === 'peel') peel = true;
      else if (arg === 'symrefs') symrefs = true;
      else if (arg === 'unborn') unborn = true;
      else if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice(11));
    }
    const wanted = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
    let out = '';
    if (wanted('HEAD')) {
      const target = `refs/heads/${repo.defaultBranch}`;
      const oid = repo.refs.get(target);
      if (oid !== undefined) out += pkt(`${oid} HEAD${symrefs ? ` symref-target:${target}` : ''}\n`);
      else if (unborn) out += pkt(`unborn HEAD${symrefs ? ` symref-target:${target}` : ''}\n`);
    }
    for (const [name, oid] of sortedRefs(repo)) {
      if (!wanted(name)) continue;
      let line = `${oid} ${name}`;
      if (peel) {
        const peeled = this.forge.store.peel(oid);
        if (peeled !== oid) line += ` peeled:${peeled}`;
      }
      out += pkt(`${line}\n`);
    }
    return out + FLUSH;
  }

  /** Parse fetch arguments shared by v0 and v2. */
  private negotiate(repo: Repo, wants: string[], haves: string[], clientShallow: Set<string>, depth: number, includeTag: boolean): { objects: string[]; shallow: string[]; unshallow: string[]; common: string[] } {
    const store = this.forge.store;
    // What the client holds: every commit reachable from its haves, stopping at its shallow boundary.
    const common: string[] = [];
    const haveCommits = new Set<string>();
    const stack: string[] = [];
    for (const have of haves) {
      if (!store.has(have)) continue;
      common.push(have);
      const peeled = store.peel(have);
      if (store.commit(peeled) !== null) stack.push(peeled);
    }
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (haveCommits.has(oid)) continue;
      const commit = store.commit(oid);
      if (commit === null) continue;
      haveCommits.add(oid);
      if (clientShallow.has(oid)) continue;
      for (const parent of commit.parents) if (!haveCommits.has(parent)) stack.push(parent);
    }

    // What to send: commits reachable from the wants (to `depth` when deepening), less what the client holds.
    const sendTags: string[] = [];
    const sendCommits: string[] = [];
    const shallow: string[] = [];
    const unshallow: string[] = [];
    const depthOf = new Map<string, number>();
    const queue: string[] = [];
    const extraObjects: string[] = [];
    for (const want of wants) {
      let oid = want;
      for (let hop = 0; hop < 16; hop += 1) {
        const obj = store.get(oid);
        if (obj === undefined) break;
        if (obj.type === OBJ_TAG) {
          if (!sendTags.includes(oid)) sendTags.push(oid);
          oid = store.tag(oid)!.object;
          continue;
        }
        if (obj.type === OBJ_COMMIT) {
          if (!depthOf.has(oid)) {
            depthOf.set(oid, 1);
            queue.push(oid);
          }
        } else if (!extraObjects.includes(oid)) extraObjects.push(oid);
        break;
      }
    }
    for (let q = 0; q < queue.length; q += 1) {
      const oid = queue[q]!;
      const d = depthOf.get(oid)!;
      const commit = store.commit(oid)!;
      const had = haveCommits.has(oid);
      if (!had) sendCommits.push(oid);
      if (depth > 0) {
        if (d >= depth) {
          if (commit.parents.length > 0) shallow.push(oid);
          continue;
        }
        if (clientShallow.has(oid) && commit.parents.length > 0) unshallow.push(oid);
      } else if (had) {
        continue;
      }
      if (depth === 0 && clientShallow.has(oid)) continue;
      for (const parent of commit.parents) {
        if (depthOf.has(parent) || store.commit(parent) === null) continue;
        depthOf.set(parent, d + 1);
        queue.push(parent);
      }
    }
    // A client-shallow commit that is still a boundary stays shallow: report nothing for it.
    const shallowList = shallow.filter((oid) => !clientShallow.has(oid));

    // Trees and blobs the client already holds (from the commits it has).
    const haveObjects = new Set<string>();
    const markTree = (tree: string): void => {
      if (haveObjects.has(tree)) return;
      haveObjects.add(tree);
      const entries = store.tree(tree);
      if (entries === null) return;
      for (const entry of entries) {
        if (entry.mode === '160000') continue;
        if (entry.mode === '40000') markTree(entry.oid);
        else haveObjects.add(entry.oid);
      }
    };
    if (haveCommits.size > 0) {
      for (const oid of haveCommits) {
        const commit = store.commit(oid);
        if (commit !== null) markTree(commit.tree);
      }
    }
    const objects: string[] = [];
    const sent = new Set<string>();
    const addTree = (tree: string): void => {
      if (sent.has(tree) || haveObjects.has(tree)) return;
      sent.add(tree);
      objects.push(tree);
      const entries = store.tree(tree);
      if (entries === null) return;
      for (const entry of entries) {
        if (entry.mode === '160000') continue;
        if (entry.mode === '40000') addTree(entry.oid);
        else if (!sent.has(entry.oid) && !haveObjects.has(entry.oid) && store.has(entry.oid)) {
          sent.add(entry.oid);
          objects.push(entry.oid);
        }
      }
    };
    for (const oid of sendCommits) {
      sent.add(oid);
      objects.push(oid);
    }
    for (const oid of sendCommits) addTree(store.commit(oid)!.tree);
    for (const oid of sendTags) if (!sent.has(oid)) {
      sent.add(oid);
      objects.push(oid);
    }
    for (const oid of extraObjects) {
      if (sent.has(oid)) continue;
      const obj = store.get(oid)!;
      if (obj.type === OBJ_TREE) addTree(oid);
      else {
        sent.add(oid);
        objects.push(oid);
      }
    }
    if (includeTag) {
      for (const [name, oid] of repo.refs) {
        if (!name.startsWith('refs/tags/') || sent.has(oid)) continue;
        if (store.get(oid)?.type !== OBJ_TAG) continue;
        const target = store.peel(oid);
        if (sent.has(target) || haveCommits.has(target)) {
          sent.add(oid);
          objects.push(oid);
        }
      }
    }
    return { objects, shallow: shallowList, unshallow, common };
  }

  private fetchV2(repo: Repo, args: string[]): Buffer {
    const wants: string[] = [];
    const haves: string[] = [];
    const clientShallow = new Set<string>();
    const wantedRefs: string[] = [];
    let depth = 0;
    let done = false;
    let includeTag = false;
    for (const arg of args) {
      if (arg.startsWith('want ')) wants.push(arg.slice(5, 45));
      else if (arg.startsWith('have ')) haves.push(arg.slice(5, 45));
      else if (arg.startsWith('shallow ')) clientShallow.add(arg.slice(8, 48));
      else if (arg.startsWith('deepen ')) depth = Math.max(0, Number.parseInt(arg.slice(7), 10) || 0);
      else if (arg.startsWith('want-ref ')) {
        const ref = arg.slice(9);
        const oid = repo.refs.get(ref);
        if (oid !== undefined) {
          wants.push(oid);
          wantedRefs.push(`${oid} ${ref}`);
        }
      } else if (arg === 'done') done = true;
      else if (arg === 'include-tag') includeTag = true;
    }
    const result = this.negotiate(repo, wants, haves, clientShallow, depth, includeTag);
    const parts: Buffer[] = [];
    let head = '';
    if (!done) {
      head += pkt('acknowledgments\n');
      if (result.common.length === 0) head += pkt('NAK\n');
      for (const oid of result.common) head += pkt(`ACK ${oid}\n`);
      head += pkt('ready\n');
      head += DELIM;
    }
    if (depth > 0 || clientShallow.size > 0) {
      head += pkt('shallow-info\n');
      for (const oid of result.shallow) head += pkt(`shallow ${oid}\n`);
      for (const oid of result.unshallow) head += pkt(`unshallow ${oid}\n`);
      head += DELIM;
    }
    if (wantedRefs.length > 0) {
      head += pkt('wanted-refs\n');
      for (const line of wantedRefs) head += pkt(`${line}\n`);
      head += DELIM;
    }
    head += pkt('packfile\n');
    parts.push(Buffer.from(head, 'utf8'));
    const pack = writePack(this.forge.store, result.objects);
    parts.push(...sideband(pack));
    parts.push(Buffer.from(FLUSH, 'latin1'));
    return Buffer.concat(parts);
  }

  private uploadPackV0(repo: Repo, body: Buffer): Response {
    const cacheKey = `v0\0${repo.id}\0${body.toString('latin1')}`;
    const cached = this.packCache.get(cacheKey);
    if (cached !== undefined && cached.version === repo.version) return { status: 200, type: UPLOAD_RESULT, body: cached.body, extra: NO_CACHE };
    const { packets } = readPackets(body);
    const wants: string[] = [];
    const haves: string[] = [];
    const clientShallow = new Set<string>();
    let depth = 0;
    let done = false;
    let sidebandMode = 0;
    let includeTag = false;
    let flushes = 0;
    for (const packet of packets) {
      if (packet.kind === 'flush') {
        flushes += 1;
        continue;
      }
      const text = packetText(packet);
      if (text.startsWith('want ')) {
        wants.push(text.slice(5, 45));
        const caps = text.slice(46).split(' ');
        if (caps.includes('side-band-64k')) sidebandMode = 2;
        else if (caps.includes('side-band') && sidebandMode === 0) sidebandMode = 1;
        if (caps.includes('include-tag')) includeTag = true;
      } else if (text.startsWith('have ')) haves.push(text.slice(5, 45));
      else if (text.startsWith('shallow ')) clientShallow.add(text.slice(8, 48));
      else if (text.startsWith('deepen ')) depth = Math.max(0, Number.parseInt(text.slice(7), 10) || 0);
      else if (text === 'done') done = true;
    }
    const result = this.negotiate(repo, wants, haves, clientShallow, depth, includeTag);
    let head = '';
    if (depth > 0 || clientShallow.size > 0) {
      for (const oid of result.shallow) head += pkt(`shallow ${oid}\n`);
      for (const oid of result.unshallow) head += pkt(`unshallow ${oid}\n`);
      head += FLUSH;
    }
    if (!done && flushes <= 1 && haves.length === 0 && depth > 0) {
      // The stateless first round of a shallow fetch: the shallow update only.
      return { status: 200, type: UPLOAD_RESULT, body: head, extra: NO_CACHE };
    }
    if (!done) {
      // Without multi_ack the server acknowledges the first common commit or says NAK, then sends nothing more.
      head += result.common.length > 0 ? pkt(`ACK ${result.common[0]}\n`) : pkt('NAK\n');
      if (haves.length > 0) return { status: 200, type: UPLOAD_RESULT, body: head, extra: NO_CACHE };
    } else {
      head += result.common.length > 0 ? pkt(`ACK ${result.common[result.common.length - 1]}\n`) : pkt('NAK\n');
    }
    const pack = writePack(this.forge.store, result.objects);
    const parts: Buffer[] = [Buffer.from(head, 'utf8')];
    if (sidebandMode === 0) parts.push(pack);
    else {
      if (sidebandMode === 1) {
        for (let i = 0; i < pack.length; i += 995) parts.push(pktBuffer(Buffer.concat([Buffer.from([1]), pack.subarray(i, Math.min(pack.length, i + 995))])));
      } else parts.push(...sideband(pack));
      parts.push(Buffer.from(FLUSH, 'latin1'));
    }
    const out = Buffer.concat(parts);
    if (this.packCache.size > 256) this.packCache.clear();
    this.packCache.set(cacheKey, { version: repo.version, body: out });
    return { status: 200, type: UPLOAD_RESULT, body: out, extra: NO_CACHE };
  }

  // ---------------------------------------------------------------- receive-pack

  receivePack(repo: Repo, body: Buffer): Response {
    const forge = this.forge;
    const store = forge.store;
    let packets: Packet[];
    let rest: number;
    try {
      ({ packets, rest } = readPackets(body, 0, true));
    } catch {
      return { status: 400, type: 'text/plain; charset=utf-8', body: 'malformed receive-pack request\n' };
    }
    const commands: Array<{ old: string; next: string; ref: string }> = [];
    let caps: string[] = [];
    for (const packet of packets) {
      if (packet.kind !== 'data') continue;
      let text = packetText(packet);
      const nul = text.indexOf('\0');
      if (nul >= 0) {
        caps = text.slice(nul + 1).split(' ').filter(Boolean);
        text = text.slice(0, nul);
      }
      if (text.startsWith('shallow ')) continue;
      const parts = text.split(' ');
      if (parts.length < 3 || !/^[0-9a-f]{40}$/.test(parts[0]!) || !/^[0-9a-f]{40}$/.test(parts[1]!)) {
        return { status: 400, type: 'text/plain; charset=utf-8', body: 'malformed receive-pack command\n' };
      }
      commands.push({ old: parts[0]!, next: parts[1]!, ref: parts.slice(2).join(' ') });
    }
    if (commands.length === 0 && packets.length === 0) {
      return { status: 400, type: 'text/plain; charset=utf-8', body: 'empty receive-pack request\n' };
    }
    const useSideband = caps.includes('side-band-64k') || caps.includes('side-band');
    const reportStatus = caps.includes('report-status') || caps.includes('report-status-v2');

    let unpack = 'ok';
    const packData = body.subarray(rest);
    if (packData.length > 0) {
      try {
        const objects = readPack(store, packData);
        for (const object of objects) store.putKnown(object.oid, object.type, object.data);
      } catch (err) {
        unpack = err instanceof GitError ? err.message : 'unpack failed';
      }
    }

    const results: string[] = [];
    for (const command of commands) {
      const { old, next, ref } = command;
      if (unpack !== 'ok') {
        results.push(`ng ${ref} unpacker error`);
        continue;
      }
      if (!ref.startsWith('refs/') || ref.includes('..') || ref.endsWith('/') || ref.endsWith('.lock')) {
        results.push(`ng ${ref} funny refname`);
        continue;
      }
      const current = repo.refs.get(ref) ?? ZERO_OID;
      if (next === ZERO_OID) {
        if (ref === `refs/heads/${repo.defaultBranch}` && repo.refs.size > 1) {
          results.push(`ng ${ref} deletion of the current branch prohibited`);
          continue;
        }
        if (old !== ZERO_OID && current !== old) {
          results.push(`ng ${ref} stale info`);
          continue;
        }
        forge.setRef(repo, ref, null);
        results.push(`ok ${ref}`);
        continue;
      }
      if (current !== old) {
        results.push(current !== ZERO_OID && old !== ZERO_OID && !isAncestor(store, current, next) ? `ng ${ref} non-fast-forward` : `ng ${ref} stale info`);
        continue;
      }
      const target = store.get(next);
      if (target === undefined) {
        results.push(`ng ${ref} missing necessary objects`);
        continue;
      }
      if (ref.startsWith('refs/heads/') && store.peel(next) !== next && store.get(store.peel(next))?.type !== OBJ_COMMIT) {
        results.push(`ng ${ref} not a commit`);
        continue;
      }
      if (ref.startsWith('refs/heads/') && target.type !== OBJ_COMMIT) {
        results.push(`ng ${ref} not a commit`);
        continue;
      }
      if (current !== ZERO_OID && ref.startsWith('refs/heads/') && !isAncestor(store, current, next)) {
        results.push(`ng ${ref} non-fast-forward`);
        continue;
      }
      forge.setRef(repo, ref, next);
      results.push(`ok ${ref}`);
    }

    if (!reportStatus) return { status: 200, type: RECEIVE_RESULT, body: '', extra: NO_CACHE };
    let report = pkt(`unpack ${unpack}\n`);
    for (const line of results) report += pkt(`${line}\n`);
    report += FLUSH;
    if (!useSideband) return { status: 200, type: RECEIVE_RESULT, body: report, extra: NO_CACHE };
    const framed = Buffer.concat([...sideband(Buffer.from(report, 'utf8')), Buffer.from(FLUSH, 'latin1')]);
    return { status: 200, type: RECEIVE_RESULT, body: framed, extra: NO_CACHE };
  }
}
