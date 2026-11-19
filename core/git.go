// Git object model: the content-addressed object store, commit/tree/tag parsing, tree building,
// packfile reading (with OFS/REF delta resolution) and writing, and pkt-line framing.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const ZeroOid = "0000000000000000000000000000000000000000"

const (
	ObjCommit   = 1
	ObjTree     = 2
	ObjBlob     = 3
	ObjTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

var typeNames = []string{"", "commit", "tree", "blob", "tag"}

type Signature struct {
	Name  string
	Email string
	Time  int64
	TZ    string
}

type Commit struct {
	Tree      string
	Parents   []string
	Author    Signature
	Committer Signature
	Message   string
}

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

type Tag struct {
	Object  string
	Type    string
	Tag     string
	Message string
}

type GitObject struct {
	Type   int
	Data   []byte
	packed []byte
	commit *Commit
	tree   []TreeEntry
	tag    *Tag
}

type GitError struct{ msg string }

func (e *GitError) Error() string { return e.msg }

func gitErr(format string, args ...any) error { return &GitError{fmt.Sprintf(format, args...)} }

// ------------------------------------------------------------------ hashing and the store

func hashObject(typ int, data []byte) string {
	h := sha1.New()
	var head [32]byte
	b := append(head[:0], typeNames[typ]...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, int64(len(data)), 10)
	b = append(b, 0)
	h.Write(b)
	h.Write(data)
	var sum [20]byte
	return hex.EncodeToString(h.Sum(sum[:0]))
}

// ObjectStore is one object store shared by every repository (objects are content-addressed).
type ObjectStore struct {
	objects map[string]*GitObject
}

func NewObjectStore() *ObjectStore { return &ObjectStore{objects: make(map[string]*GitObject, 4096)} }

func (s *ObjectStore) Has(oid string) bool { _, ok := s.objects[oid]; return ok }

func (s *ObjectStore) Get(oid string) *GitObject { return s.objects[oid] }

func (s *ObjectStore) PutKnown(oid string, typ int, data []byte) {
	if _, ok := s.objects[oid]; !ok {
		s.objects[oid] = &GitObject{Type: typ, Data: data}
	}
}

func (s *ObjectStore) Put(typ int, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s.objects[oid]; !ok {
		s.objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

func (s *ObjectStore) Commit(oid string) *Commit {
	obj := s.objects[oid]
	if obj == nil || obj.Type != ObjCommit {
		return nil
	}
	if obj.commit == nil {
		obj.commit = parseCommit(obj.Data)
	}
	return obj.commit
}

func (s *ObjectStore) Tree(oid string) ([]TreeEntry, bool) {
	obj := s.objects[oid]
	if obj == nil || obj.Type != ObjTree {
		return nil, false
	}
	if obj.tree == nil {
		entries, err := parseTree(obj.Data)
		if err != nil {
			return nil, false
		}
		if entries == nil {
			entries = []TreeEntry{}
		}
		obj.tree = entries
	}
	return obj.tree, true
}

func (s *ObjectStore) Tag(oid string) *Tag {
	obj := s.objects[oid]
	if obj == nil || obj.Type != ObjTag {
		return nil
	}
	if obj.tag == nil {
		obj.tag = parseTag(obj.Data)
	}
	return obj.tag
}

// Peel follows annotated tags down to the first non-tag object.
func (s *ObjectStore) Peel(oid string) string {
	current := oid
	for i := 0; i < 16; i++ {
		tag := s.Tag(current)
		if tag == nil {
			return current
		}
		current = tag.Object
	}
	return current
}

var deflaters = sync.Pool{New: func() any {
	w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
	return w
}}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data)/2 + 64)
	w := deflaters.Get().(*zlib.Writer)
	w.Reset(&buf)
	w.Write(data)
	w.Close()
	deflaters.Put(w)
	return buf.Bytes()
}

// Packed is the pack-ready bytes of one stored object: header plus deflated data, built once.
func (s *ObjectStore) Packed(oid string) ([]byte, error) {
	obj := s.objects[oid]
	if obj == nil {
		return nil, gitErr("missing object %s", oid)
	}
	if obj.packed == nil {
		head := packObjectHeader(obj.Type, len(obj.Data))
		z := deflate(obj.Data)
		out := make([]byte, 0, len(head)+len(z))
		out = append(out, head...)
		out = append(out, z...)
		obj.packed = out
	}
	return obj.packed, nil
}

// ------------------------------------------------------------------ parsing

func parseSignature(text string) Signature {
	lt := strings.IndexByte(text, '<')
	gt := -1
	if lt >= 0 {
		if i := strings.IndexByte(text[lt+1:], '>'); i >= 0 {
			gt = lt + 1 + i
		}
	}
	if lt < 0 || gt < 0 {
		return Signature{Name: strings.TrimSpace(text), TZ: "+0000"}
	}
	name := strings.TrimSpace(text[:lt])
	email := text[lt+1 : gt]
	rest := strings.Split(strings.TrimSpace(text[gt+1:]), " ")
	var t int64
	if len(rest) > 0 {
		t, _ = strconv.ParseInt(rest[0], 10, 64)
	}
	tz := "+0000"
	if len(rest) > 1 {
		tz = rest[1]
	}
	return Signature{Name: name, Email: email, Time: t, TZ: tz}
}

func parseCommit(data []byte) *Commit {
	text := string(data)
	head, message := text, ""
	if split := strings.Index(text, "\n\n"); split >= 0 {
		head, message = text[:split], text[split+2:]
	}
	c := &Commit{Author: Signature{TZ: "+0000"}, Parents: []string{}}
	c.Committer = c.Author
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = clip(line, 5, 45)
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, clip(line, 7, 47))
		case strings.HasPrefix(line, "author "):
			c.Author = parseSignature(line[7:])
		case strings.HasPrefix(line, "committer "):
			c.Committer = parseSignature(line[10:])
		}
	}
	c.Message = message
	return c
}

func clip(s string, from, to int) string {
	if to > len(s) {
		to = len(s)
	}
	if from > to {
		return ""
	}
	return s[from:to]
}

func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	p := 0
	for p < len(data) {
		space := bytes.IndexByte(data[p:], ' ')
		if space < 0 {
			return nil, gitErr("corrupt tree: no mode terminator")
		}
		space += p
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, gitErr("corrupt tree: truncated entry")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, gitErr("corrupt tree: truncated entry")
		}
		entries = append(entries, TreeEntry{Mode: string(data[p:space]), Name: string(data[space+1 : nul]), Oid: hex.EncodeToString(data[nul+1 : nul+21])})
		p = nul + 21
	}
	return entries, nil
}

func parseTag(data []byte) *Tag {
	text := string(data)
	head, message := text, ""
	if split := strings.Index(text, "\n\n"); split >= 0 {
		head, message = text[:split], text[split+2:]
	}
	t := &Tag{Message: message}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			t.Object = clip(line, 7, 47)
		case strings.HasPrefix(line, "type "):
			t.Type = line[5:]
		case strings.HasPrefix(line, "tag "):
			t.Tag = line[4:]
		}
	}
	return t
}

// ------------------------------------------------------------------ building objects

func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func serializeTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return treeSortKey(sorted[i]) < treeSortKey(sorted[j]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.Oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

func formatSignature(sig Signature) string {
	return sig.Name + " <" + sig.Email + "> " + strconv.FormatInt(sig.Time, 10) + " " + sig.TZ
}

func serializeCommit(c *Commit) []byte {
	var b strings.Builder
	b.WriteString("tree ")
	b.WriteString(c.Tree)
	b.WriteByte('\n')
	for _, p := range c.Parents {
		b.WriteString("parent ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString("author ")
	b.WriteString(formatSignature(c.Author))
	b.WriteString("\ncommitter ")
	b.WriteString(formatSignature(c.Committer))
	b.WriteString("\n\n")
	b.WriteString(c.Message)
	return []byte(b.String())
}

type FileEntry struct {
	Mode string
	Oid  string
}

// FlatFiles is an insertion-ordered path -> entry map (iteration order matters for stable output).
type FlatFiles struct {
	Keys []string
	M    map[string]FileEntry
}

func NewFlatFiles() *FlatFiles { return &FlatFiles{M: map[string]FileEntry{}} }

func (f *FlatFiles) Set(path string, e FileEntry) {
	if _, ok := f.M[path]; !ok {
		f.Keys = append(f.Keys, path)
	}
	f.M[path] = e
}

func (f *FlatFiles) Delete(path string) {
	if _, ok := f.M[path]; !ok {
		return
	}
	delete(f.M, path)
	for i, k := range f.Keys {
		if k == path {
			f.Keys = append(f.Keys[:i:i], f.Keys[i+1:]...)
			break
		}
	}
}

func (f *FlatFiles) Get(path string) (FileEntry, bool) { e, ok := f.M[path]; return e, ok }

func (f *FlatFiles) Clone() *FlatFiles {
	out := &FlatFiles{Keys: append([]string(nil), f.Keys...), M: make(map[string]FileEntry, len(f.M))}
	for k, v := range f.M {
		out.M[k] = v
	}
	return out
}

// flattenTree maps every blob (and gitlink) path below a tree to its mode and oid.
func flattenTree(store *ObjectStore, treeOid string, prefix string, out *FlatFiles) *FlatFiles {
	if out == nil {
		out = NewFlatFiles()
	}
	entries, ok := store.Tree(treeOid)
	if !ok {
		return out
	}
	for _, e := range entries {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			flattenTree(store, e.Oid, path, out)
		} else {
			out.Set(path, FileEntry{Mode: e.Mode, Oid: e.Oid})
		}
	}
	return out
}

type dirNode struct {
	files   []TreeEntry
	dirKeys []string
	dirs    map[string]*dirNode
}

// buildTree builds (and stores) a tree hierarchy from a flat path map; returns the root tree id.
func buildTree(store *ObjectStore, files *FlatFiles) string {
	root := &dirNode{dirs: map[string]*dirNode{}}
	for _, path := range files.Keys {
		e := files.M[path]
		parts := strings.Split(path, "/")
		dir := root
		for i := 0; i < len(parts)-1; i++ {
			name := parts[i]
			next := dir.dirs[name]
			if next == nil {
				next = &dirNode{dirs: map[string]*dirNode{}}
				dir.dirs[name] = next
				dir.dirKeys = append(dir.dirKeys, name)
			}
			dir = next
		}
		dir.files = append(dir.files, TreeEntry{Mode: e.Mode, Name: parts[len(parts)-1], Oid: e.Oid})
	}
	var write func(d *dirNode) string
	write = func(d *dirNode) string {
		entries := append([]TreeEntry(nil), d.files...)
		for _, name := range d.dirKeys {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: write(d.dirs[name])})
		}
		return store.Put(ObjTree, serializeTree(entries))
	}
	return write(root)
}

// lookupPath finds the entry at path below a root tree: a blob/gitlink entry, a tree entry, or nil.
func lookupPath(store *ObjectStore, treeOid string, path string) *TreeEntry {
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return &TreeEntry{Mode: "40000", Name: "", Oid: treeOid}
	}
	current := treeOid
	for i, part := range parts {
		entries, ok := store.Tree(current)
		if !ok {
			return nil
		}
		var found *TreeEntry
		for j := range entries {
			if entries[j].Name == part {
				e := entries[j]
				found = &e
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(parts)-1 {
			return found
		}
		if found.Mode != "40000" {
			return nil
		}
		current = found.Oid
	}
	return nil
}

// ------------------------------------------------------------------ history walks

func isAncestor(store *ObjectStore, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		c := store.Commit(oid)
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if !seen[p] {
				stack = append(stack, p)
			}
		}
	}
	return false
}

func ancestry(store *ObjectStore, start string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		c := store.Commit(oid)
		if c == nil {
			continue
		}
		seen[oid] = true
		for _, p := range c.Parents {
			if !seen[p] {
				stack = append(stack, p)
			}
		}
	}
	return seen
}

func mergeBase(store *ObjectStore, a, b string) string {
	fromA := ancestry(store, a)
	if fromA[b] {
		return b
	}
	seen := map[string]bool{}
	queue := []string{b}
	for i := 0; i < len(queue); i++ {
		oid := queue[i]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if fromA[oid] {
			return oid
		}
		c := store.Commit(oid)
		if c == nil {
			continue
		}
		queue = append(queue, c.Parents...)
	}
	return ""
}

// logFrom lists commits reachable from start, newest first (committer time, then discovery order).
func logFrom(store *ObjectStore, start string, limit int, exclude map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	heap := []string{start}
	for len(heap) > 0 && len(out) < limit {
		best := 0
		for i := 1; i < len(heap); i++ {
			ci := store.Commit(heap[i])
			cb := store.Commit(heap[best])
			var ti, tb int64
			if ci != nil {
				ti = ci.Committer.Time
			}
			if cb != nil {
				tb = cb.Committer.Time
			}
			if ti > tb {
				best = i
			}
		}
		oid := heap[best]
		heap = append(heap[:best:best], heap[best+1:]...)
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if exclude != nil && exclude[oid] {
			continue
		}
		c := store.Commit(oid)
		if c == nil {
			continue
		}
		out = append(out, oid)
		for _, p := range c.Parents {
			if !seen[p] {
				heap = append(heap, p)
			}
		}
	}
	return out
}

// ------------------------------------------------------------------ packfiles

func packObjectHeader(typ int, size int) []byte {
	var out []byte
	c := byte(typ<<4) | byte(size&0x0f)
	rest := size >> 4
	for rest > 0 {
		out = append(out, c|0x80)
		c = byte(rest & 0x7f)
		rest >>= 7
	}
	return append(out, c)
}

// writePack builds a version-2 pack of the given stored objects, undeltified, with its SHA-1 trailer.
func writePack(store *ObjectStore, oids []string) ([]byte, error) {
	total := 12 + 20
	parts := make([][]byte, 0, len(oids))
	for _, oid := range oids {
		p, err := store.Packed(oid)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
		total += len(p)
	}
	out := make([]byte, 12, total)
	copy(out, "PACK")
	binary.BigEndian.PutUint32(out[4:], 2)
	binary.BigEndian.PutUint32(out[8:], uint32(len(oids)))
	for _, p := range parts {
		out = append(out, p...)
	}
	sum := sha1.Sum(out)
	return append(out, sum[:]...), nil
}

func readDeltaSize(delta []byte, p int) (int, int, error) {
	size, shift := 0, uint(0)
	for {
		if p >= len(delta) {
			return 0, 0, gitErr("corrupt delta header")
		}
		c := delta[p]
		p++
		size |= int(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			break
		}
	}
	return size, p, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	baseSize, p, err := readDeltaSize(delta, 0)
	if err != nil {
		return nil, err
	}
	if baseSize != len(base) {
		return nil, gitErr("delta base size mismatch")
	}
	resultSize, p, err := readDeltaSize(delta, p)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, resultSize)
	for p < len(delta) {
		op := delta[p]
		p++
		if op&0x80 != 0 {
			offset, length := 0, 0
			for i, bit := range []byte{0x01, 0x02, 0x04, 0x08} {
				if op&bit != 0 {
					if p >= len(delta) {
						return nil, gitErr("corrupt delta")
					}
					offset |= int(delta[p]) << (8 * i)
					p++
				}
			}
			for i, bit := range []byte{0x10, 0x20, 0x40} {
				if op&bit != 0 {
					if p >= len(delta) {
						return nil, gitErr("corrupt delta")
					}
					length |= int(delta[p]) << (8 * i)
					p++
				}
			}
			if length == 0 {
				length = 0x10000
			}
			if offset+length > len(base) || len(out)+length > resultSize {
				return nil, gitErr("delta copy out of range")
			}
			out = append(out, base[offset:offset+length]...)
		} else if op != 0 {
			n := int(op)
			if p+n > len(delta) || len(out)+n > resultSize {
				return nil, gitErr("delta insert out of range")
			}
			out = append(out, delta[p:p+n]...)
			p += n
		} else {
			return nil, gitErr("delta opcode 0")
		}
	}
	if len(out) != resultSize {
		return nil, gitErr("delta result size mismatch")
	}
	return out, nil
}

type IngestedObject struct {
	Oid  string
	Type int
	Data []byte
}

type rawObject struct {
	offset     int
	typ        int
	data       []byte
	baseOffset int
	baseOid    string
	resolved   *IngestedObject
}

var inflaters sync.Pool

// inflateAt inflates one zlib stream at the front of src and reports the bytes it consumed.
func inflateAt(src []byte, sizeHint int) ([]byte, int, error) {
	br := bytes.NewReader(src)
	var zr io.ReadCloser
	if v := inflaters.Get(); v != nil {
		zr = v.(io.ReadCloser)
		if err := zr.(zlib.Resetter).Reset(br, nil); err != nil {
			return nil, 0, gitErr("corrupt zlib stream")
		}
	} else {
		r, err := zlib.NewReader(br)
		if err != nil {
			return nil, 0, gitErr("corrupt zlib stream")
		}
		zr = r
	}
	out := make([]byte, 0, sizeHint)
	buf := bytes.NewBuffer(out)
	if _, err := io.Copy(buf, zr); err != nil {
		return nil, 0, gitErr("corrupt zlib stream")
	}
	inflaters.Put(zr)
	return buf.Bytes(), len(src) - br.Len(), nil
}

// readPack parses a pack and resolves every delta; REF_DELTA bases may be in the pack or the store.
func readPack(store *ObjectStore, pack []byte) ([]IngestedObject, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, gitErr("not a pack")
	}
	version := binary.BigEndian.Uint32(pack[4:])
	if version != 2 && version != 3 {
		return nil, gitErr("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:]))
	end := len(pack) - 20
	sum := sha1.Sum(pack[:end])
	if !bytes.Equal(sum[:], pack[end:]) {
		return nil, gitErr("pack checksum mismatch")
	}
	raws := make([]*rawObject, 0, count)
	byOffset := make(map[int]*rawObject, count)
	p := 12
	for i := 0; i < count; i++ {
		if p >= end {
			return nil, gitErr("pack truncated")
		}
		offset := p
		c := pack[p]
		p++
		typ := int(c>>4) & 7
		size := int(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if p >= end {
				return nil, gitErr("pack truncated")
			}
			c = pack[p]
			p++
			size |= int(c&0x7f) << shift
			shift += 7
		}
		raw := &rawObject{offset: offset, typ: typ, baseOffset: -1}
		switch {
		case typ == objOfsDelta:
			if p >= end {
				return nil, gitErr("pack truncated")
			}
			c = pack[p]
			p++
			back := int(c & 0x7f)
			for c&0x80 != 0 {
				if p >= end {
					return nil, gitErr("pack truncated")
				}
				c = pack[p]
				p++
				back = (back+1)<<7 | int(c&0x7f)
			}
			raw.baseOffset = offset - back
		case typ == objRefDelta:
			if p+20 > end {
				return nil, gitErr("pack truncated")
			}
			raw.baseOid = hex.EncodeToString(pack[p : p+20])
			p += 20
		case typ < 1 || typ > 4:
			return nil, gitErr("bad pack object type %d", typ)
		}
		data, used, err := inflateAt(pack[p:end], size)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, gitErr("pack object size mismatch")
		}
		raw.data = data
		p += used
		raws = append(raws, raw)
		byOffset[offset] = raw
	}

	byOid := make(map[string]*IngestedObject, count)
	var resolve func(raw *rawObject, depth int) (*IngestedObject, error)
	resolve = func(raw *rawObject, depth int) (*IngestedObject, error) {
		if raw.resolved != nil {
			return raw.resolved, nil
		}
		if depth > 10000 {
			return nil, gitErr("delta chain too deep")
		}
		var baseType int
		var baseData []byte
		switch raw.typ {
		case objOfsDelta:
			base := byOffset[raw.baseOffset]
			if base == nil {
				return nil, gitErr("ofs-delta base not found")
			}
			rb, err := resolve(base, depth+1)
			if err != nil || rb == nil {
				return rb, err
			}
			baseType, baseData = rb.Type, rb.Data
		case objRefDelta:
			if in := byOid[raw.baseOid]; in != nil {
				baseType, baseData = in.Type, in.Data
			} else if stored := store.Get(raw.baseOid); stored != nil {
				baseType, baseData = stored.Type, stored.Data
			} else {
				return nil, nil
			}
		default:
			obj := &IngestedObject{Oid: hashObject(raw.typ, raw.data), Type: raw.typ, Data: raw.data}
			raw.resolved = obj
			byOid[obj.Oid] = obj
			return obj, nil
		}
		data, err := applyDelta(baseData, raw.data)
		if err != nil {
			return nil, err
		}
		obj := &IngestedObject{Oid: hashObject(baseType, data), Type: baseType, Data: data}
		raw.resolved = obj
		byOid[obj.Oid] = obj
		return obj, nil
	}
	var pending []*rawObject
	for _, raw := range raws {
		r, err := resolve(raw, 0)
		if err != nil {
			return nil, err
		}
		if r == nil {
			pending = append(pending, raw)
		}
	}
	for len(pending) > 0 {
		var next []*rawObject
		for _, raw := range pending {
			r, err := resolve(raw, 0)
			if err != nil {
				return nil, err
			}
			if r == nil {
				next = append(next, raw)
			}
		}
		if len(next) == len(pending) {
			return nil, gitErr("unresolved delta base %s", next[0].baseOid)
		}
		pending = next
	}
	out := make([]IngestedObject, len(raws))
	for i, raw := range raws {
		out[i] = *raw.resolved
	}
	return out, nil
}

// ------------------------------------------------------------------ pkt-lines

const hexDigits = "0123456789abcdef"

func pktHeader(length int) []byte {
	return []byte{hexDigits[(length>>12)&15], hexDigits[(length>>8)&15], hexDigits[(length>>4)&15], hexDigits[length&15]}
}

func appendPkt(dst []byte, text string) []byte {
	dst = append(dst, pktHeader(len(text)+4)...)
	return append(dst, text...)
}

func appendPktBytes(dst []byte, data []byte) []byte {
	dst = append(dst, pktHeader(len(data)+4)...)
	return append(dst, data...)
}

const pktFlush = "0000"
const pktDelim = "0001"

// appendSideband frames data on a band in pkt-lines of at most 65520 bytes.
func appendSideband(dst []byte, data []byte, band byte) []byte {
	const max = 65515
	for i := 0; i < len(data); i += max {
		j := i + max
		if j > len(data) {
			j = len(data)
		}
		dst = append(dst, pktHeader(j-i+5)...)
		dst = append(dst, band)
		dst = append(dst, data[i:j]...)
	}
	return dst
}

const (
	pktData = iota
	pktKindFlush
	pktKindDelim
	pktKindEnd
)

type Packet struct {
	Kind    int
	Payload []byte
}

var errBadPkt = errors.New("bad pkt-line length")

// readPackets splits a request body into pkt-lines; rest is the offset after the last parsed packet.
func readPackets(body []byte, start int, stopAfterFlush bool) ([]Packet, int, error) {
	var packets []Packet
	p := start
	for p+4 <= len(body) {
		n, err := strconv.ParseUint(string(body[p:p+4]), 16, 32)
		if err != nil {
			return nil, 0, &GitError{errBadPkt.Error()}
		}
		length := int(n)
		switch length {
		case 0:
			packets = append(packets, Packet{Kind: pktKindFlush})
			p += 4
			if stopAfterFlush {
				return packets, p, nil
			}
			continue
		case 1:
			packets = append(packets, Packet{Kind: pktKindDelim})
			p += 4
			continue
		case 2:
			packets = append(packets, Packet{Kind: pktKindEnd})
			p += 4
			continue
		}
		if length < 4 || p+length > len(body) {
			return nil, 0, gitErr("truncated pkt-line")
		}
		packets = append(packets, Packet{Kind: pktData, Payload: body[p+4 : p+length]})
		p += length
	}
	return packets, p, nil
}

// packetText is a data packet's text without its trailing newline.
func packetText(pk Packet) string {
	if pk.Kind != pktData {
		return ""
	}
	s := string(pk.Payload)
	return strings.TrimSuffix(s, "\n")
}
