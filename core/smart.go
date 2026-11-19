// Git smart HTTP: info/refs advertisements, upload-pack (protocol v2 ls-refs and fetch, plus v0 fetch)
// and receive-pack (v0, report-status over side-band-64k). Pack answers are cached per repository
// version, so repeated identical fetches (a clone storm) cost one map lookup each.
package main

import (
	"sort"
	"strconv"
	"strings"
)

const gitAgent = "git/forge-go-1.0"
const uploadAdvert = "application/x-git-upload-pack-advertisement"
const receiveAdvert = "application/x-git-receive-pack-advertisement"
const uploadResult = "application/x-git-upload-pack-result"
const receiveResult = "application/x-git-receive-pack-result"
const noCache = "Cache-Control: no-cache, max-age=0, must-revalidate\r\nExpires: Fri, 01 Jan 1980 00:00:00 GMT\r\nPragma: no-cache\r\n"

var v2Capabilities = func() []byte {
	var b []byte
	b = appendPkt(b, "version 2\n")
	b = appendPkt(b, "agent="+gitAgent+"\n")
	b = appendPkt(b, "ls-refs=unborn\n")
	b = appendPkt(b, "fetch=shallow wait-for-done\n")
	b = appendPkt(b, "server-option\n")
	b = appendPkt(b, "object-format=sha1\n")
	return append(b, pktFlush...)
}()

type cacheEntry struct {
	version int
	body    []byte
}

func textResp(status int, body string) *Response {
	return &Response{Status: status, Type: "text/plain; charset=utf-8", Body: []byte(body)}
}

func sortedRefNames(r *Repo) []string {
	names := append([]string(nil), r.Refs.Keys()...)
	sort.Strings(names)
	return names
}

type SmartHTTP struct {
	forge     *Forge
	packCache map[string]cacheEntry
}

func NewSmartHTTP(f *Forge) *SmartHTTP {
	return &SmartHTTP{forge: f, packCache: map[string]cacheEntry{}}
}

func (s *SmartHTTP) result(body []byte) *Response {
	return &Response{Status: 200, Type: uploadResult, Body: body, Extra: noCache}
}

// ---------------------------------------------------------------- advertisements

func (s *SmartHTTP) infoRefs(r *Repo, service, gitProtocol string) *Response {
	if service == "git-upload-pack" {
		if strings.Contains(gitProtocol, "version=2") {
			return &Response{Status: 200, Type: uploadAdvert, Body: v2Capabilities, Extra: noCache}
		}
		return &Response{Status: 200, Type: uploadAdvert, Body: s.v0Advertisement(r, true), Extra: noCache}
	}
	return &Response{Status: 200, Type: receiveAdvert, Body: s.v0Advertisement(r, false), Extra: noCache}
}

func (s *SmartHTTP) v0Advertisement(r *Repo, upload bool) []byte {
	var body []byte
	if upload {
		body = appendPkt(body, "# service=git-upload-pack\n")
	} else {
		body = appendPkt(body, "# service=git-receive-pack\n")
	}
	body = append(body, pktFlush...)
	symref := "symref=HEAD:refs/heads/" + r.DefaultBranch
	var caps string
	if upload {
		caps = "multi_ack_detailed no-done side-band-64k side-band thin-pack ofs-delta shallow no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want " + symref + " object-format=sha1 agent=" + gitAgent
	} else {
		caps = "report-status delete-refs side-band-64k quiet ofs-delta object-format=sha1 agent=" + gitAgent
	}
	var lines []string
	if upload {
		if headOid, ok := r.Refs.Get("refs/heads/" + r.DefaultBranch); ok {
			lines = append(lines, headOid+" HEAD")
		}
	}
	for _, name := range sortedRefNames(r) {
		oid, _ := r.Refs.Get(name)
		lines = append(lines, oid+" "+name)
		if upload && strings.HasPrefix(name, "refs/tags/") {
			if peeled := s.forge.store.Peel(oid); peeled != oid {
				lines = append(lines, peeled+" "+name+"^{}")
			}
		}
	}
	if len(lines) == 0 {
		body = appendPkt(body, ZeroOid+" capabilities^{}\x00"+caps+"\n")
	} else {
		body = appendPkt(body, lines[0]+"\x00"+caps+"\n")
		for _, line := range lines[1:] {
			body = appendPkt(body, line+"\n")
		}
	}
	return append(body, pktFlush...)
}

// ---------------------------------------------------------------- upload-pack

func (s *SmartHTTP) uploadPack(r *Repo, body []byte, gitProtocol string) (*Response, error) {
	if strings.Contains(gitProtocol, "version=2") {
		return s.uploadPackV2(r, body)
	}
	return s.uploadPackV0(r, body)
}

func (s *SmartHTTP) remember(key string, r *Repo, out []byte) {
	if len(s.packCache) > 256 {
		s.packCache = map[string]cacheEntry{}
	}
	s.packCache[key] = cacheEntry{version: r.Version, body: out}
}

func (s *SmartHTTP) uploadPackV2(r *Repo, body []byte) (*Response, error) {
	cacheKey := strconv.Itoa(r.ID) + "\x00" + string(body)
	if cached, ok := s.packCache[cacheKey]; ok && cached.version == r.Version {
		return s.result(cached.body), nil
	}
	packets, _, err := readPackets(body, 0, false)
	if err != nil {
		return nil, err
	}
	command := ""
	i := 0
	for ; i < len(packets); i++ {
		if packets[i].Kind != pktData {
			break
		}
		text := packetText(packets[i])
		if strings.HasPrefix(text, "command=") {
			command = text[8:]
		}
	}
	if i < len(packets) && packets[i].Kind == pktKindDelim {
		i++
	}
	var args []string
	for ; i < len(packets); i++ {
		if packets[i].Kind != pktData {
			break
		}
		args = append(args, packetText(packets[i]))
	}
	var out []byte
	switch {
	case command == "ls-refs":
		out = s.lsRefs(r, args)
	case command == "fetch":
		out, err = s.fetchV2(r, args)
		if err != nil {
			return nil, err
		}
	case command == "" && len(packets) == 0:
		out = []byte(pktFlush)
	default:
		return textResp(400, "unknown command "+command+"\n"), nil
	}
	s.remember(cacheKey, r, out)
	return s.result(out), nil
}

func (s *SmartHTTP) lsRefs(r *Repo, args []string) []byte {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case arg == "unborn":
			unborn = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, arg[11:])
		}
	}
	wanted := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out []byte
	if wanted("HEAD") {
		target := "refs/heads/" + r.DefaultBranch
		sym := ""
		if symrefs {
			sym = " symref-target:" + target
		}
		if oid, ok := r.Refs.Get(target); ok {
			out = appendPkt(out, oid+" HEAD"+sym+"\n")
		} else if unborn {
			out = appendPkt(out, "unborn HEAD"+sym+"\n")
		}
	}
	for _, name := range sortedRefNames(r) {
		if !wanted(name) {
			continue
		}
		oid, _ := r.Refs.Get(name)
		line := oid + " " + name
		if peel {
			if peeled := s.forge.store.Peel(oid); peeled != oid {
				line += " peeled:" + peeled
			}
		}
		out = appendPkt(out, line+"\n")
	}
	return append(out, pktFlush...)
}

type negotiation struct {
	objects   []string
	shallow   []string
	unshallow []string
	common    []string
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (s *SmartHTTP) negotiate(r *Repo, wants, haves []string, clientShallow map[string]bool, depth int, includeTag bool) negotiation {
	store := s.forge.store
	// What the client holds: every commit reachable from its haves, stopping at its shallow boundary.
	var common []string
	haveCommits := map[string]bool{}
	var stack []string
	for _, have := range haves {
		if !store.Has(have) {
			continue
		}
		common = append(common, have)
		peeled := store.Peel(have)
		if store.Commit(peeled) != nil {
			stack = append(stack, peeled)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if haveCommits[oid] {
			continue
		}
		c := store.Commit(oid)
		if c == nil {
			continue
		}
		haveCommits[oid] = true
		if clientShallow[oid] {
			continue
		}
		for _, p := range c.Parents {
			if !haveCommits[p] {
				stack = append(stack, p)
			}
		}
	}

	// What to send: commits reachable from the wants (to depth when deepening), less what the client holds.
	var sendTags, sendCommits, shallow, unshallow, queue, extraObjects []string
	depthOf := map[string]int{}
	for _, want := range wants {
		oid := want
		for hop := 0; hop < 16; hop++ {
			obj := store.Get(oid)
			if obj == nil {
				break
			}
			if obj.Type == ObjTag {
				if !containsStr(sendTags, oid) {
					sendTags = append(sendTags, oid)
				}
				oid = store.Tag(oid).Object
				continue
			}
			if obj.Type == ObjCommit {
				if _, ok := depthOf[oid]; !ok {
					depthOf[oid] = 1
					queue = append(queue, oid)
				}
			} else if !containsStr(extraObjects, oid) {
				extraObjects = append(extraObjects, oid)
			}
			break
		}
	}
	for q := 0; q < len(queue); q++ {
		oid := queue[q]
		d := depthOf[oid]
		c := store.Commit(oid)
		had := haveCommits[oid]
		if !had {
			sendCommits = append(sendCommits, oid)
		}
		if depth > 0 {
			if d >= depth {
				if len(c.Parents) > 0 {
					shallow = append(shallow, oid)
				}
				continue
			}
			if clientShallow[oid] && len(c.Parents) > 0 {
				unshallow = append(unshallow, oid)
			}
		} else if had {
			continue
		}
		if depth == 0 && clientShallow[oid] {
			continue
		}
		for _, p := range c.Parents {
			if _, ok := depthOf[p]; ok || store.Commit(p) == nil {
				continue
			}
			depthOf[p] = d + 1
			queue = append(queue, p)
		}
	}
	var shallowList []string
	for _, oid := range shallow {
		if !clientShallow[oid] {
			shallowList = append(shallowList, oid)
		}
	}

	// Trees and blobs the client already holds (from the commits it has).
	haveObjects := map[string]bool{}
	var markTree func(tree string)
	markTree = func(tree string) {
		if haveObjects[tree] {
			return
		}
		haveObjects[tree] = true
		entries, ok := store.Tree(tree)
		if !ok {
			return
		}
		for _, e := range entries {
			if e.Mode == "160000" {
				continue
			}
			if e.Mode == "40000" {
				markTree(e.Oid)
			} else {
				haveObjects[e.Oid] = true
			}
		}
	}
	for oid := range haveCommits {
		if c := store.Commit(oid); c != nil {
			markTree(c.Tree)
		}
	}
	objects := make([]string, 0, 1024)
	sent := map[string]bool{}
	var addTree func(tree string)
	addTree = func(tree string) {
		if sent[tree] || haveObjects[tree] {
			return
		}
		sent[tree] = true
		objects = append(objects, tree)
		entries, ok := store.Tree(tree)
		if !ok {
			return
		}
		for _, e := range entries {
			if e.Mode == "160000" {
				continue
			}
			if e.Mode == "40000" {
				addTree(e.Oid)
			} else if !sent[e.Oid] && !haveObjects[e.Oid] && store.Has(e.Oid) {
				sent[e.Oid] = true
				objects = append(objects, e.Oid)
			}
		}
	}
	for _, oid := range sendCommits {
		sent[oid] = true
		objects = append(objects, oid)
	}
	for _, oid := range sendCommits {
		addTree(store.Commit(oid).Tree)
	}
	for _, oid := range sendTags {
		if !sent[oid] {
			sent[oid] = true
			objects = append(objects, oid)
		}
	}
	for _, oid := range extraObjects {
		if sent[oid] {
			continue
		}
		if store.Get(oid).Type == ObjTree {
			addTree(oid)
		} else {
			sent[oid] = true
			objects = append(objects, oid)
		}
	}
	if includeTag {
		for _, name := range r.Refs.Keys() {
			oid, _ := r.Refs.Get(name)
			if !strings.HasPrefix(name, "refs/tags/") || sent[oid] {
				continue
			}
			if obj := store.Get(oid); obj == nil || obj.Type != ObjTag {
				continue
			}
			target := store.Peel(oid)
			if sent[target] || haveCommits[target] {
				sent[oid] = true
				objects = append(objects, oid)
			}
		}
	}
	return negotiation{objects: objects, shallow: shallowList, unshallow: unshallow, common: common}
}

func parseDepth(text string) int {
	n, ok := jsParseInt(text)
	if !ok || n < 0 {
		return 0
	}
	return n
}

func slice(s string, from, to int) string {
	if from > len(s) {
		return ""
	}
	if to > len(s) {
		to = len(s)
	}
	return s[from:to]
}

func (s *SmartHTTP) fetchV2(r *Repo, args []string) ([]byte, error) {
	var wants, haves, wantedRefs []string
	clientShallow := map[string]bool{}
	depth := 0
	done, includeTag := false, false
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			wants = append(wants, slice(arg, 5, 45))
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, slice(arg, 5, 45))
		case strings.HasPrefix(arg, "shallow "):
			clientShallow[slice(arg, 8, 48)] = true
		case strings.HasPrefix(arg, "deepen "):
			depth = parseDepth(arg[7:])
		case strings.HasPrefix(arg, "want-ref "):
			ref := arg[9:]
			if oid, ok := r.Refs.Get(ref); ok {
				wants = append(wants, oid)
				wantedRefs = append(wantedRefs, oid+" "+ref)
			}
		case arg == "done":
			done = true
		case arg == "include-tag":
			includeTag = true
		}
	}
	res := s.negotiate(r, wants, haves, clientShallow, depth, includeTag)
	var out []byte
	if !done {
		out = appendPkt(out, "acknowledgments\n")
		if len(res.common) == 0 {
			out = appendPkt(out, "NAK\n")
		}
		for _, oid := range res.common {
			out = appendPkt(out, "ACK "+oid+"\n")
		}
		out = appendPkt(out, "ready\n")
		out = append(out, pktDelim...)
	}
	if depth > 0 || len(clientShallow) > 0 {
		out = appendPkt(out, "shallow-info\n")
		for _, oid := range res.shallow {
			out = appendPkt(out, "shallow "+oid+"\n")
		}
		for _, oid := range res.unshallow {
			out = appendPkt(out, "unshallow "+oid+"\n")
		}
		out = append(out, pktDelim...)
	}
	if len(wantedRefs) > 0 {
		out = appendPkt(out, "wanted-refs\n")
		for _, line := range wantedRefs {
			out = appendPkt(out, line+"\n")
		}
		out = append(out, pktDelim...)
	}
	out = appendPkt(out, "packfile\n")
	pack, err := writePack(s.forge.store, res.objects)
	if err != nil {
		return nil, err
	}
	framed := make([]byte, 0, len(out)+len(pack)+len(pack)/65515*5+16)
	framed = append(framed, out...)
	framed = appendSideband(framed, pack, 1)
	return append(framed, pktFlush...), nil
}

func (s *SmartHTTP) uploadPackV0(r *Repo, body []byte) (*Response, error) {
	cacheKey := "v0\x00" + strconv.Itoa(r.ID) + "\x00" + string(body)
	if cached, ok := s.packCache[cacheKey]; ok && cached.version == r.Version {
		return s.result(cached.body), nil
	}
	packets, _, err := readPackets(body, 0, false)
	if err != nil {
		return nil, err
	}
	var wants, haves []string
	clientShallow := map[string]bool{}
	depth, sidebandMode, flushes := 0, 0, 0
	done, includeTag := false, false
	for _, pk := range packets {
		if pk.Kind == pktKindFlush {
			flushes++
			continue
		}
		text := packetText(pk)
		switch {
		case strings.HasPrefix(text, "want "):
			wants = append(wants, slice(text, 5, 45))
			caps := strings.Split(slice(text, 46, len(text)), " ")
			if containsStr(caps, "side-band-64k") {
				sidebandMode = 2
			} else if containsStr(caps, "side-band") && sidebandMode == 0 {
				sidebandMode = 1
			}
			if containsStr(caps, "include-tag") {
				includeTag = true
			}
		case strings.HasPrefix(text, "have "):
			haves = append(haves, slice(text, 5, 45))
		case strings.HasPrefix(text, "shallow "):
			clientShallow[slice(text, 8, 48)] = true
		case strings.HasPrefix(text, "deepen "):
			depth = parseDepth(text[7:])
		case text == "done":
			done = true
		}
	}
	res := s.negotiate(r, wants, haves, clientShallow, depth, includeTag)
	var head []byte
	if depth > 0 || len(clientShallow) > 0 {
		for _, oid := range res.shallow {
			head = appendPkt(head, "shallow "+oid+"\n")
		}
		for _, oid := range res.unshallow {
			head = appendPkt(head, "unshallow "+oid+"\n")
		}
		head = append(head, pktFlush...)
	}
	if !done && flushes <= 1 && len(haves) == 0 && depth > 0 {
		// The stateless first round of a shallow fetch: the shallow update only.
		return s.result(head), nil
	}
	if !done {
		if len(res.common) > 0 {
			head = appendPkt(head, "ACK "+res.common[0]+"\n")
		} else {
			head = appendPkt(head, "NAK\n")
		}
		if len(haves) > 0 {
			return s.result(head), nil
		}
	} else if len(res.common) > 0 {
		head = appendPkt(head, "ACK "+res.common[len(res.common)-1]+"\n")
	} else {
		head = appendPkt(head, "NAK\n")
	}
	pack, err := writePack(s.forge.store, res.objects)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(head)+len(pack)+len(pack)/900*5+16)
	out = append(out, head...)
	switch sidebandMode {
	case 0:
		out = append(out, pack...)
	case 1:
		for i := 0; i < len(pack); i += 995 {
			j := i + 995
			if j > len(pack) {
				j = len(pack)
			}
			out = append(out, pktHeader(j-i+5)...)
			out = append(out, 1)
			out = append(out, pack[i:j]...)
		}
		out = append(out, pktFlush...)
	default:
		out = appendSideband(out, pack, 1)
		out = append(out, pktFlush...)
	}
	s.remember(cacheKey, r, out)
	return s.result(out), nil
}

// ---------------------------------------------------------------- receive-pack

func isFullSha(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

type pushCommand struct{ old, next, ref string }

func (s *SmartHTTP) receivePack(r *Repo, body []byte) *Response {
	f := s.forge
	store := f.store
	packets, rest, err := readPackets(body, 0, true)
	if err != nil {
		return textResp(400, "malformed receive-pack request\n")
	}
	var commands []pushCommand
	var caps []string
	for _, pk := range packets {
		if pk.Kind != pktData {
			continue
		}
		text := packetText(pk)
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			caps = caps[:0]
			for _, c := range strings.Split(text[nul+1:], " ") {
				if c != "" {
					caps = append(caps, c)
				}
			}
			text = text[:nul]
		}
		if strings.HasPrefix(text, "shallow ") {
			continue
		}
		parts := strings.Split(text, " ")
		if len(parts) < 3 || !isFullSha(parts[0]) || !isFullSha(parts[1]) {
			return textResp(400, "malformed receive-pack command\n")
		}
		commands = append(commands, pushCommand{old: parts[0], next: parts[1], ref: strings.Join(parts[2:], " ")})
	}
	if len(commands) == 0 && len(packets) == 0 {
		return textResp(400, "empty receive-pack request\n")
	}
	useSideband := containsStr(caps, "side-band-64k") || containsStr(caps, "side-band")
	reportStatus := containsStr(caps, "report-status") || containsStr(caps, "report-status-v2")

	unpack := "ok"
	if rest < len(body) {
		objects, err := readPack(store, body[rest:])
		if err != nil {
			if ge, ok := err.(*GitError); ok {
				unpack = ge.msg
			} else {
				unpack = "unpack failed"
			}
		} else {
			for _, o := range objects {
				store.PutKnown(o.Oid, o.Type, o.Data)
			}
		}
	}

	results := make([]string, 0, len(commands))
	for _, cmd := range commands {
		old, next, ref := cmd.old, cmd.next, cmd.ref
		if unpack != "ok" {
			results = append(results, "ng "+ref+" unpacker error")
			continue
		}
		if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") {
			results = append(results, "ng "+ref+" funny refname")
			continue
		}
		current, ok := r.Refs.Get(ref)
		if !ok {
			current = ZeroOid
		}
		if next == ZeroOid {
			if ref == "refs/heads/"+r.DefaultBranch && r.Refs.Len() > 1 {
				results = append(results, "ng "+ref+" deletion of the current branch prohibited")
				continue
			}
			if old != ZeroOid && current != old {
				results = append(results, "ng "+ref+" stale info")
				continue
			}
			f.setRef(r, ref, "")
			results = append(results, "ok "+ref)
			continue
		}
		if current != old {
			if current != ZeroOid && old != ZeroOid && !isAncestor(store, current, next) {
				results = append(results, "ng "+ref+" non-fast-forward")
			} else {
				results = append(results, "ng "+ref+" stale info")
			}
			continue
		}
		target := store.Get(next)
		if target == nil {
			results = append(results, "ng "+ref+" missing necessary objects")
			continue
		}
		isHead := strings.HasPrefix(ref, "refs/heads/")
		if isHead {
			if peeled := store.Peel(next); peeled != next {
				if po := store.Get(peeled); po == nil || po.Type != ObjCommit {
					results = append(results, "ng "+ref+" not a commit")
					continue
				}
			}
			if target.Type != ObjCommit {
				results = append(results, "ng "+ref+" not a commit")
				continue
			}
			if current != ZeroOid && !isAncestor(store, current, next) {
				results = append(results, "ng "+ref+" non-fast-forward")
				continue
			}
		}
		f.setRef(r, ref, next)
		results = append(results, "ok "+ref)
	}

	if !reportStatus {
		return &Response{Status: 200, Type: receiveResult, Body: []byte{}, Extra: noCache}
	}
	report := appendPkt(nil, "unpack "+unpack+"\n")
	for _, line := range results {
		report = appendPkt(report, line+"\n")
	}
	report = append(report, pktFlush...)
	if !useSideband {
		return &Response{Status: 200, Type: receiveResult, Body: report, Extra: noCache}
	}
	framed := appendSideband(nil, report, 1)
	framed = append(framed, pktFlush...)
	return &Response{Status: 200, Type: receiveResult, Body: framed, Extra: noCache}
}
