package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const gitZero = "0000000000000000000000000000000000000000"
const gitHead = "refs/heads/main"
const gitMaxPacket = 65520
const gitMaxObject = 64 * 1024 * 1024
const gitMaxExpandedPack = 256 * 1024 * 1024
const gitMaxEntries = 500000

var gitOID = regexp.MustCompile(`^[0-9a-f]{40}$`)
var gitHex4 = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)
var gitDigits = regexp.MustCompile(`^[0-9]+$`)
var gitProtocolV2 = regexp.MustCompile(`(?:^|:)\s*version=2(?:\s*:|\s*$)`)
var gitBadRef = regexp.MustCompile(`\.\.|@\{|[\x00-\x20\x7f~^:?*\[\\]`)
var gitCommandRef = regexp.MustCompile(`[\x00-\x20\x7f]`)
var gitTypes = []string{"", "commit", "tree", "blob", "tag"}

type GitObject struct {
	Type string
	Data []byte
}

type gitStoredObject struct {
	GitObject
	compressed []byte
	compressMu sync.Mutex
}
type GitFile struct{ Sha, Mode string }
type GitCommit struct {
	Sha, Tree string
	Parents   []string
	Message   string
}
type GitStore struct {
	Refs      map[string]string
	RefOrder  []string
	objects   map[string]*gitStoredObject
	connected map[string]bool
	uploads   uploadCache
}

func NewGitStore() *GitStore {
	return &GitStore{Refs: map[string]string{}, RefOrder: []string{}, objects: map[string]*gitStoredObject{}, connected: map[string]bool{}}
}
func gitAssert(ok bool, message string) {
	if !ok {
		panic(message)
	}
}
func gitValidType(t string) bool { return t == "commit" || t == "tree" || t == "blob" || t == "tag" }
func gitHash(t string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", t, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
func gitReadOID(s string) string {
	gitAssert(gitOID.MatchString(s) && s != gitZero, "invalid object ID")
	return s
}
func (s *GitStore) lookup(id string) *gitStoredObject { return s.objects[id] }
func (s *GitStore) Write(t string, data []byte) string {
	gitAssert(gitValidType(t) && len(data) <= gitMaxObject, "invalid object")
	id := gitHash(t, data)
	if s.objects[id] == nil {
		s.objects[id] = &gitStoredObject{GitObject: GitObject{Type: t, Data: bytes.Clone(data)}}
	}
	return id
}
func (s *GitStore) Read(id string) *GitObject {
	o := s.lookup(id)
	if o == nil {
		return nil
	}
	return &GitObject{Type: o.Type, Data: bytes.Clone(o.Data)}
}
func (s *GitStore) SetRef(ref, id string) {
	if _, ok := s.Refs[ref]; !ok {
		s.RefOrder = append(s.RefOrder, ref)
	}
	s.Refs[ref] = id
}
func (s *GitStore) DeleteRef(ref string) {
	if _, ok := s.Refs[ref]; !ok {
		return
	}
	delete(s.Refs, ref)
	for i, x := range s.RefOrder {
		if x == ref {
			s.RefOrder = append(s.RefOrder[:i], s.RefOrder[i+1:]...)
			break
		}
	}
}
func gitHeaders(data []byte) ([]string, string) {
	i := bytes.Index(data, []byte("\n\n"))
	gitAssert(i >= 0, "missing object header separator")
	return strings.Split(string(data[:i]), "\n"), string(data[i+2:])
}
func gitCommitParts(data []byte) GitCommit {
	h, m := gitHeaders(data)
	gitAssert(strings.HasPrefix(h[0], "tree "), "missing commit tree")
	c := GitCommit{Tree: gitReadOID(h[0][5:]), Parents: []string{}, Message: m}
	for _, line := range h {
		if strings.HasPrefix(line, "parent ") {
			c.Parents = append(c.Parents, gitReadOID(line[7:]))
		}
	}
	return c
}

type gitLink struct{ sha, typ string }

func gitTagTarget(data []byte) gitLink {
	h, _ := gitHeaders(data)
	gitAssert(len(h) > 1 && strings.HasPrefix(h[0], "object ") && strings.HasPrefix(h[1], "type "), "invalid tag headers")
	typ := h[1][5:]
	gitAssert(gitValidType(typ), "invalid tag target type")
	return gitLink{gitReadOID(h[0][7:]), typ}
}
func gitValidName(n string) bool {
	l := strings.ToLower(n)
	return len(n) > 0 && !strings.ContainsAny(n, "\x00/") && l != "." && l != ".." && l != ".git"
}
func gitFileMode(m string) bool {
	return m == "100644" || m == "100755" || m == "120000" || m == "160000"
}

type gitTreeEntry struct {
	name string
	GitFile
}

func gitTreeEntries(data []byte) []gitTreeEntry {
	result := []gitTreeEntry{}
	for p := 0; p < len(data); {
		a := bytes.IndexByte(data[p:], ' ')
		b := bytes.IndexByte(data[p:], 0)
		gitAssert(a > 0 && b > a && p+b+21 <= len(data), "truncated tree entry")
		a += p
		b += p
		m, n := string(data[p:a]), string(data[a+1:b])
		gitAssert((m == "40000" || gitFileMode(m)) && gitValidName(n), "invalid tree entry")
		result = append(result, gitTreeEntry{n, GitFile{gitReadOID(hex.EncodeToString(data[b+1 : b+21])), m}})
		p = b + 21
	}
	return result
}
func gitLinks(o *gitStoredObject, cut bool) []gitLink {
	switch o.Type {
	case "blob":
		return nil
	case "tag":
		return []gitLink{gitTagTarget(o.Data)}
	case "tree":
		a := []gitLink{}
		for _, e := range gitTreeEntries(o.Data) {
			if e.Mode == "160000" {
				continue
			}
			typ := "blob"
			if e.Mode == "40000" {
				typ = "tree"
			}
			a = append(a, gitLink{e.Sha, typ})
		}
		return a
	case "commit":
		c := gitCommitParts(o.Data)
		a := []gitLink{{c.Tree, "tree"}}
		if !cut {
			for _, p := range c.Parents {
				a = append(a, gitLink{p, "commit"})
			}
		}
		return a
	}
	panic("invalid object type")
}

// Ordered sets keep JavaScript Set's insertion order observable in pack traversal.
type gitSet struct {
	ids []string
	has map[string]bool
}

func gitNewSet(ids ...string) *gitSet {
	s := &gitSet{has: map[string]bool{}}
	for _, id := range ids {
		s.add(id)
	}
	return s
}
func (s *gitSet) add(id string) {
	if !s.has[id] {
		s.has[id] = true
		s.ids = append(s.ids, id)
	}
}
func (s *gitSet) del(id string) {
	if !s.has[id] {
		return
	}
	delete(s.has, id)
	for i, v := range s.ids {
		if v == id {
			s.ids = append(s.ids[:i], s.ids[i+1:]...)
			break
		}
	}
}
func gitWalk(roots []string, lookup func(string) *gitStoredObject, cut, omit map[string]bool) *gitSet {
	seen := gitNewSet()
	todo := make([]gitLink, 0, len(roots))
	for _, id := range roots {
		todo = append(todo, gitLink{id, ""})
	}
	for len(todo) > 0 {
		last := len(todo) - 1
		e := todo[last]
		todo = todo[:last]
		o := lookup(e.sha)
		gitAssert(o != nil && (e.typ == "" || o.Type == e.typ), "missing or wrong-type object")
		if omit[e.sha] || seen.has[e.sha] {
			continue
		}
		seen.add(e.sha)
		todo = append(todo, gitLinks(o, cut[e.sha])...)
	}
	return seen
}
func gitPeel(id string, lookup func(string) *gitStoredObject) string {
	start := id
	seen := map[string]bool{}
	o := lookup(id)
	for o != nil && o.Type == "tag" {
		gitAssert(!seen[id], "tag cycle")
		seen[id] = true
		id = gitTagTarget(o.Data).sha
		o = lookup(id)
		gitAssert(o != nil, "missing tag target")
	}
	if id == start {
		return ""
	}
	return id
}
func gitAncestor(a, b string, lookup func(string) *gitStoredObject) bool {
	todo := []string{b}
	seen := map[string]bool{}
	for len(todo) > 0 {
		i := len(todo) - 1
		id := todo[i]
		todo = todo[:i]
		if seen[id] {
			continue
		}
		seen[id] = true
		o := lookup(id)
		if o == nil || o.Type != "commit" {
			continue
		}
		if id == a {
			return true
		}
		todo = append(todo, gitCommitParts(o.Data).Parents...)
	}
	return false
}
func AncestorOf(a, b string, s *GitStore) bool  { return gitAncestor(a, b, s.lookup) }
func (s *GitStore) IsAncestor(a, b string) bool { return AncestorOf(a, b, s) }
func (s *GitStore) Commit(files map[string]GitFile, parents []string, message string) string {
	type dir struct {
		dirs  map[string]*dir
		files map[string]GitFile
	}
	fresh := func() *dir { return &dir{map[string]*dir{}, map[string]GitFile{}} }
	root := fresh()
	for _, p := range parents {
		o := s.lookup(p)
		gitAssert(o != nil && o.Type == "commit", "missing commit parent")
	}
	for path, f := range files {
		names := strings.Split(path, "/")
		for _, n := range names {
			gitAssert(gitValidName(n), "invalid file path")
		}
		gitAssert(gitFileMode(f.Mode), "invalid file mode")
		gitReadOID(f.Sha)
		o := s.lookup(f.Sha)
		gitAssert(f.Mode == "160000" || (o != nil && o.Type == "blob"), "missing file blob")
		d := root
		for _, n := range names[:len(names)-1] {
			_, conflict := d.files[n]
			gitAssert(!conflict, "file/directory conflict")
			if d.dirs[n] == nil {
				d.dirs[n] = fresh()
			}
			d = d.dirs[n]
		}
		leaf := names[len(names)-1]
		gitAssert(d.dirs[leaf] == nil, "file/directory conflict")
		d.files[leaf] = f
	}
	var encode func(*dir) string
	encode = func(d *dir) string {
		entries := []gitTreeEntry{}
		for n, f := range d.files {
			entries = append(entries, gitTreeEntry{n, f})
		}
		for n, sub := range d.dirs {
			entries = append(entries, gitTreeEntry{n, GitFile{encode(sub), "40000"}})
		}
		sort.Slice(entries, func(i, j int) bool {
			a, b := entries[i].name, entries[j].name
			if entries[i].Mode == "40000" {
				a += "/"
			}
			if entries[j].Mode == "40000" {
				b += "/"
			}
			return a < b
		})
		var buf bytes.Buffer
		for _, e := range entries {
			fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.name)
			id, _ := hex.DecodeString(e.Sha)
			buf.Write(id)
		}
		return s.Write("tree", buf.Bytes())
	}
	tree := encode(root)
	h := []string{"tree " + tree}
	for _, p := range parents {
		h = append(h, "parent "+p)
	}
	who := "Forge <forge@localhost> 1700000000 +0000"
	h = append(h, "author "+who, "committer "+who)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return s.Write("commit", []byte(strings.Join(h, "\n")+"\n\n"+message))
}
func (s *GitStore) Files(id string) map[string]GitFile {
	result := map[string]GitFile{}
	o := s.lookup(id)
	if o == nil || o.Type != "commit" {
		return result
	}
	type node struct{ id, prefix string }
	todo := []node{{gitCommitParts(o.Data).Tree, ""}}
	for len(todo) > 0 {
		i := len(todo) - 1
		n := todo[i]
		todo = todo[:i]
		o = s.lookup(n.id)
		gitAssert(o != nil && o.Type == "tree", "missing tree")
		for _, e := range gitTreeEntries(o.Data) {
			path := n.prefix + e.name
			if e.Mode == "40000" {
				todo = append(todo, node{e.Sha, path + "/"})
			} else {
				result[path] = e.GitFile
			}
		}
	}
	return result
}
func (s *GitStore) History(id string) []GitCommit {
	r := []GitCommit{}
	seen := map[string]bool{}
	todo := []string{id}
	for len(todo) > 0 {
		i := len(todo) - 1
		id = todo[i]
		todo = todo[:i]
		if seen[id] {
			continue
		}
		seen[id] = true
		o := s.lookup(id)
		if o == nil || o.Type != "commit" {
			continue
		}
		c := gitCommitParts(o.Data)
		c.Sha = id
		c.Message = strings.TrimSuffix(c.Message, "\n")
		r = append(r, c)
		for j := len(c.Parents) - 1; j >= 0; j-- {
			todo = append(todo, c.Parents[j])
		}
	}
	return r
}

type gitCursor struct {
	data   []byte
	p, end int
}

func gitCursorFor(data []byte) *gitCursor { return &gitCursor{data, 0, len(data)} }
func (c *gitCursor) take(n int) []byte {
	gitAssert(n >= 0 && n <= c.end-c.p, "truncated data")
	b := c.data[c.p : c.p+n]
	c.p += n
	return b
}
func (c *gitCursor) byte() int { return int(c.take(1)[0]) }
func (c *gitCursor) varint(low, scale int, more bool) int {
	for more {
		n := c.byte()
		digit := n & 127
		gitAssert(digit <= (gitMaxObject-low)/scale, "object too large")
		low += digit * scale
		more = n&128 != 0
		if more {
			gitAssert(scale <= gitMaxObject/128, "object too large")
			scale *= 128
		}
	}
	return low
}
func gitApplyDelta(base, delta []byte, charge func(int)) []byte {
	c := gitCursorFor(delta)
	gitAssert(c.varint(0, 1, true) == len(base), "delta base size mismatch")
	size := c.varint(0, 1, true)
	charge(size)
	result := make([]byte, size)
	p := 0
	for c.p < c.end {
		op := c.byte()
		if op&128 != 0 {
			var offset uint32
			n := 0
			for i := 0; i < 4; i++ {
				if op&(1<<i) != 0 {
					offset |= uint32(c.byte()) << (8 * i)
				}
			}
			for i := 0; i < 3; i++ {
				if op&(16<<i) != 0 {
					n += c.byte() << (8 * i)
				}
			}
			if n == 0 {
				n = 65536
			}
			gitAssert(uint64(offset)+uint64(n) <= uint64(len(base)) && p+n <= len(result), "delta copy out of range")
			copy(result[p:], base[int(offset):int(offset)+n])
			p += n
		} else {
			gitAssert(op > 0 && p+op <= len(result), "bad delta insertion")
			copy(result[p:], c.take(op))
			p += op
		}
	}
	gitAssert(p == len(result), "delta result size mismatch")
	return result
}

type gitPackEntry struct {
	offset, baseOffset int
	baseOID, typ       string
	data, compressed   []byte
}
type gitReady struct {
	entry *gitPackEntry
	base  *gitStoredObject
}

func gitUnpack(pack []byte, external func(string) *gitStoredObject) map[string]*gitStoredObject {
	gitAssert(len(pack) >= 32 && string(pack[:4]) == "PACK", "invalid PACK header")
	version, count := binary.BigEndian.Uint32(pack[4:8]), binary.BigEndian.Uint32(pack[8:12])
	gitAssert((version == 2 || version == 3) && count <= gitMaxEntries, "unsupported PACK header")
	end := len(pack) - 20
	sum := sha1.Sum(pack[:end])
	gitAssert(bytes.Equal(sum[:], pack[end:]), "pack checksum mismatch")
	c := &gitCursor{pack, 12, end}
	objects := map[string]*gitStoredObject{}
	offsets := map[int]*gitStoredObject{}
	pendingOffset := map[int][]*gitPackEntry{}
	pendingOID := map[string][]*gitPackEntry{}
	ready := []gitReady{}
	expanded := 0
	charge := func(n int) { expanded += n; gitAssert(expanded <= gitMaxExpandedPack, "expanded pack too large") }
	save := func(e *gitPackEntry, o *gitStoredObject) {
		id := gitHash(o.Type, o.Data)
		objects[id] = o
		offsets[e.offset] = o
		if e.baseOffset == 0 && e.baseOID == "" {
			o.compressed = e.compressed
		}
		for _, w := range pendingOID[id] {
			ready = append(ready, gitReady{w, o})
		}
		delete(pendingOID, id)
		for _, w := range pendingOffset[e.offset] {
			ready = append(ready, gitReady{w, o})
		}
		delete(pendingOffset, e.offset)
	}
	// bytes.Reader implements io.ByteReader so zlib cannot read ahead into
	// the next pack entry. Reuse the inflater across this pack's members.
	var reader bytes.Reader
	var inflater io.ReadCloser
	for index := uint32(0); index < count; index++ {
		offset := c.p
		first := c.byte()
		code := (first >> 4) & 7
		size := c.varint(first&15, 16, first&128 != 0)
		e := &gitPackEntry{offset: offset}
		if code == 6 {
			digit := c.byte()
			distance := int64(digit & 127)
			for digit&128 != 0 {
				digit = c.byte()
				distance = (distance+1)*128 + int64(digit&127)
				gitAssert(distance <= int64(offset-12), "invalid OFS_DELTA base")
			}
			gitAssert(distance > 0 && distance <= int64(offset-12), "invalid OFS_DELTA base")
			e.baseOffset = offset - int(distance)
		} else if code == 7 {
			e.baseOID = hex.EncodeToString(c.take(20))
		} else {
			gitAssert(code > 0 && code < len(gitTypes), "unknown pack entry type")
			e.typ = gitTypes[code]
		}
		charge(size)
		start := c.p
		reader.Reset(pack[start:end])
		var err error
		if inflater == nil {
			inflater, err = zlib.NewReader(&reader)
		} else {
			err = inflater.(zlib.Resetter).Reset(&reader, nil)
		}
		gitAssert(err == nil, "invalid zlib stream")
		data, err := io.ReadAll(io.LimitReader(inflater, int64(size)+1))
		closeErr := inflater.Close()
		gitAssert(err == nil && closeErr == nil && len(data) == size, "pack entry size mismatch")
		c.p = end - reader.Len()
		gitAssert(c.p > start, "pack entry size mismatch")
		e.data = data
		e.compressed = pack[start:c.p]
		if e.typ != "" {
			save(e, &gitStoredObject{GitObject: GitObject{Type: e.typ, Data: e.data}})
		} else {
			var base *gitStoredObject
			if e.baseOffset != 0 {
				base = offsets[e.baseOffset]
			} else {
				base = objects[e.baseOID]
				if base == nil {
					base = external(e.baseOID)
				}
			}
			if base != nil {
				ready = append(ready, gitReady{e, base})
			} else if e.baseOffset != 0 {
				pendingOffset[e.baseOffset] = append(pendingOffset[e.baseOffset], e)
			} else {
				pendingOID[e.baseOID] = append(pendingOID[e.baseOID], e)
			}
		}
		for len(ready) > 0 {
			i := len(ready) - 1
			r := ready[i]
			ready = ready[:i]
			data := gitApplyDelta(r.base.Data, r.entry.data, charge)
			save(r.entry, &gitStoredObject{GitObject: GitObject{Type: r.base.Type, Data: data}})
		}
	}
	gitAssert(c.p == end, "trailing pack bytes")
	gitAssert(len(pendingOffset) == 0 && len(pendingOID) == 0, "missing delta base")
	return objects
}

// gitCompressed publishes each immutable object's encoded bytes exactly once.
// Readers may assemble different packs concurrently without racing cache fills.
func gitCompressed(o *gitStoredObject, writer **zlib.Writer) []byte {
	o.compressMu.Lock()
	defer o.compressMu.Unlock()
	if o.compressed == nil {
		var z bytes.Buffer
		if *writer == nil {
			w, err := zlib.NewWriterLevel(&z, zlib.BestSpeed)
			gitAssert(err == nil, "compression failure")
			*writer = w
		} else {
			(*writer).Reset(&z)
		}
		_, err := (*writer).Write(o.Data)
		gitAssert(err == nil, "compression failure")
		gitAssert((*writer).Close() == nil, "compression failure")
		o.compressed = z.Bytes()
	}
	return o.compressed
}

func gitPackFor(objects []*gitStoredObject) []byte {
	var b bytes.Buffer
	var writer *zlib.Writer
	b.WriteString("PACK")
	binary.Write(&b, binary.BigEndian, uint32(2))
	binary.Write(&b, binary.BigEndian, uint32(len(objects)))
	for _, o := range objects {
		code := 0
		for i, t := range gitTypes {
			if o.Type == t {
				code = i
				break
			}
		}
		size := len(o.Data)
		header := []byte{byte((code << 4) | (size & 15))}
		size /= 16
		for size > 0 {
			header[len(header)-1] |= 128
			header = append(header, byte(size&127))
			size /= 128
		}
		b.Write(header)
		b.Write(gitCompressed(o, &writer))
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}
func gitPkt(data []byte) []byte {
	gitAssert(len(data)+4 <= gitMaxPacket, "pkt-line too large")
	return append([]byte(fmt.Sprintf("%04x", len(data)+4)), data...)
}
func gitPktText(s string) []byte { return gitPkt([]byte(s)) }
func gitPacket(c *gitCursor) (string, int) {
	p := string(c.take(4))
	gitAssert(gitHex4.MatchString(p), "invalid pkt-line prefix")
	n, err := strconv.ParseInt(p, 16, 32)
	gitAssert(err == nil, "invalid pkt-line prefix")
	if n == 0 || n == 1 {
		return "", int(n)
	}
	gitAssert(n >= 4 && n <= gitMaxPacket, "invalid pkt-line size")
	return strings.TrimSuffix(string(c.take(int(n)-4)), "\n"), -1
}
func gitSection(c *gitCursor, terminator int) []string {
	lines := []string{}
	for {
		line, control := gitPacket(c)
		if control == terminator {
			return lines
		}
		gitAssert(control == -1, "unexpected pkt-line control")
		lines = append(lines, line)
	}
}
func gitSideband(data []byte, limit int) []byte {
	var b bytes.Buffer
	for p := 0; p < len(data); p += limit - 5 {
		end := p + limit - 5
		if end > len(data) {
			end = len(data)
		}
		chunk := make([]byte, 1+end-p)
		chunk[0] = 1
		copy(chunk[1:], data[p:end])
		b.Write(gitPkt(chunk))
	}
	return b.Bytes()
}
func gitValidRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || len(ref) > 1024 || gitBadRef.MatchString(ref) || strings.HasSuffix(ref, ".") {
		return false
	}
	for _, p := range strings.Split(ref, "/") {
		if p == "" || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	return true
}
func (s *GitStore) refTips() []string {
	ids := make([]string, 0, len(s.RefOrder))
	for _, ref := range s.RefOrder {
		if id, ok := s.Refs[ref]; ok {
			ids = append(ids, id)
		}
	}
	return ids
}
func (s *GitStore) advertise(service string, v2 bool) []byte {
	upload := service == "git-upload-pack"
	var b bytes.Buffer
	if upload && v2 {
		for _, line := range []string{"version 2\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n"} {
			b.Write(gitPktText(line))
		}
		b.WriteString("0000")
		return b.Bytes()
	}
	b.Write(gitPktText("# service=" + service + "\n"))
	b.WriteString("0000")
	caps := "report-status side-band-64k delete-refs ofs-delta object-format=sha1"
	if upload {
		caps = "side-band-64k ofs-delta shallow no-progress symref=HEAD:" + gitHead + " object-format=sha1"
	}
	refs := make([]string, 0, len(s.Refs))
	for name := range s.Refs {
		refs = append(refs, name)
	}
	sort.Strings(refs)
	if upload && s.Refs[gitHead] != "" {
		refs = append([]string{"HEAD"}, refs...)
	}
	if len(refs) == 0 && !upload {
		refs = append(refs, "capabilities^{}")
	}
	for i, name := range refs {
		id := s.Refs[name]
		if name == "HEAD" {
			id = s.Refs[gitHead]
		}
		if name == "capabilities^{}" {
			id = gitZero
		}
		line := id + " " + name
		if i == 0 {
			line += "\x00" + caps
		}
		b.Write(gitPktText(line + "\n"))
		if upload && strings.HasPrefix(name, "refs/tags/") {
			target := gitPeel(id, s.lookup)
			if target != "" {
				b.Write(gitPktText(target + " " + name + "^{}\n"))
			}
		}
	}
	b.WriteString("0000")
	return b.Bytes()
}
func gitIncludes(a []string, v string) bool {
	for _, s := range a {
		if s == v {
			return true
		}
	}
	return false
}
func (s *GitStore) lsRefs(args []string) []byte {
	var b bytes.Buffer
	prefixes := []string{}
	for _, line := range args {
		if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, line[11:])
		}
	}
	accept := func(n string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				return true
			}
		}
		return false
	}
	head := s.Refs[gitHead]
	if accept("HEAD") {
		if head != "" {
			line := head + " HEAD"
			if gitIncludes(args, "symrefs") {
				line += " symref-target:" + gitHead
			}
			b.Write(gitPktText(line + "\n"))
		} else if gitIncludes(args, "unborn") {
			b.Write(gitPktText("unborn HEAD symref-target:" + gitHead + "\n"))
		}
	}
	for _, name := range s.RefOrder {
		id, ok := s.Refs[name]
		if !ok || !accept(name) {
			continue
		}
		line := id + " " + name
		if gitIncludes(args, "peel") {
			target := gitPeel(id, s.lookup)
			if target != "" {
				line += " peeled:" + target
			}
		}
		b.Write(gitPktText(line + "\n"))
	}
	b.WriteString("0000")
	return b.Bytes()
}
func (s *GitStore) fetch(args []string, v2 bool) []byte {
	wants, haves, shallow, features := gitNewSet(), gitNewSet(), gitNewSet(), gitNewSet()
	var depth uint64
	done := false
	for _, line := range args {
		parts := strings.Split(line, " ")
		key := parts[0]
		value := ""
		if len(parts) > 1 {
			value = parts[1]
		}
		switch key {
		case "want":
			wants.add(gitReadOID(value))
			if !v2 && len(parts) > 2 {
				for _, f := range parts[2:] {
					features.add(f)
				}
			}
		case "have":
			id := gitReadOID(value)
			if o := s.lookup(id); o != nil && o.Type == "commit" {
				haves.add(id)
			}
		case "shallow":
			id := gitReadOID(value)
			if o := s.lookup(id); o != nil && o.Type == "commit" {
				shallow.add(id)
			}
		case "deepen":
			n, err := strconv.ParseUint(value, 10, 53)
			gitAssert(gitDigits.MatchString(value) && err == nil && n > 0 && n <= 9007199254740991, "invalid depth")
			depth = n
		case "done":
			done = true
		case "thin-pack", "no-progress", "ofs-delta", "include-tag":
			features.add(key)
		default:
			panic("unsupported fetch argument")
		}
	}
	if len(wants.ids) == 0 {
		return []byte("0000")
	}
	tips := s.refTips()
	sendable := gitNewSet(tips...)
	for _, tip := range tips {
		if p := gitPeel(tip, s.lookup); p != "" {
			sendable.add(p)
		}
	}
	var reachable *gitSet
	for _, want := range wants.ids {
		if sendable.has[want] {
			continue
		}
		if reachable == nil {
			reachable = gitWalk(tips, s.lookup, nil, nil)
		}
		gitAssert(reachable.has[want], "want is not reachable")
	}
	var b bytes.Buffer
	if !done && v2 {
		b.Write(gitPktText("acknowledgments\n"))
		if len(haves.ids) > 0 {
			for _, id := range haves.ids {
				b.Write(gitPktText("ACK " + id + "\n"))
			}
		} else {
			b.Write(gitPktText("NAK\n"))
		}
		b.WriteString("0000")
		return b.Bytes()
	}
	boundary := gitNewSet(shallow.ids...)
	var info bytes.Buffer
	if depth > 0 {
		distances := map[string]int{}
		type distance struct {
			id string
			n  int
		}
		todo := []distance{}
		for _, id := range wants.ids {
			p := gitPeel(id, s.lookup)
			if p != "" {
				id = p
			}
			todo = append(todo, distance{id, 1})
		}
		candidates := []GitCommit{}
		for i := 0; i < len(todo); i++ {
			n := todo[i]
			if _, ok := distances[n.id]; ok {
				continue
			}
			o := s.lookup(n.id)
			if o == nil || o.Type != "commit" {
				continue
			}
			distances[n.id] = n.n
			c := gitCommitParts(o.Data)
			c.Sha = n.id
			if uint64(n.n) < depth {
				for _, p := range c.Parents {
					todo = append(todo, distance{p, n.n + 1})
				}
			} else if len(c.Parents) > 0 {
				candidates = append(candidates, c)
			}
		}
		for _, c := range candidates {
			outside := false
			for _, p := range c.Parents {
				if _, ok := distances[p]; !ok {
					outside = true
					break
				}
			}
			if !outside {
				continue
			}
			boundary.add(c.Sha)
			if !shallow.has[c.Sha] {
				info.Write(gitPktText("shallow " + c.Sha + "\n"))
			}
		}
		for _, id := range shallow.ids {
			if _, ok := distances[id]; !ok {
				continue
			}
			all := true
			for _, p := range gitCommitParts(s.lookup(id).Data).Parents {
				if _, ok := distances[p]; !ok {
					all = false
					break
				}
			}
			if all {
				boundary.del(id)
				info.Write(gitPktText("unshallow " + id + "\n"))
			}
		}
	}
	if v2 {
		if info.Len() > 0 {
			b.Write(gitPktText("shallow-info\n"))
			b.Write(info.Bytes())
			b.WriteString("0001")
		}
		b.Write(gitPktText("packfile\n"))
	} else {
		if depth > 0 {
			b.Write(info.Bytes())
			b.WriteString("0000")
		}
		if len(haves.ids) > 0 {
			b.Write(gitPktText("ACK " + haves.ids[len(haves.ids)-1] + "\n"))
		} else {
			b.Write(gitPktText("NAK\n"))
		}
		if !done {
			return b.Bytes()
		}
	}
	held := gitWalk(haves.ids, s.lookup, shallow.has, nil)
	omit := held.has
	if depth > 0 {
		omit = nil
	}
	selection := gitWalk(wants.ids, s.lookup, boundary.has, omit)
	if depth > 0 {
		kept := selection.ids[:0]
		for _, id := range selection.ids {
			if held.has[id] {
				delete(selection.has, id)
			} else {
				kept = append(kept, id)
			}
		}
		selection.ids = kept
	}
	if features.has["include-tag"] {
		for _, ref := range s.RefOrder {
			if !strings.HasPrefix(ref, "refs/tags/") {
				continue
			}
			id := s.Refs[ref]
			target := gitPeel(id, s.lookup)
			if target != "" && selection.has[target] {
				current := id
				for o := s.lookup(current); o != nil && o.Type == "tag"; o = s.lookup(current) {
					if !held.has[current] {
						selection.add(current)
					}
					current = gitTagTarget(o.Data).sha
				}
			}
		}
	}
	objects := make([]*gitStoredObject, 0, len(selection.ids))
	for _, id := range selection.ids {
		objects = append(objects, s.lookup(id))
	}
	pack := gitPackFor(objects)
	if v2 || features.has["side-band-64k"] || features.has["side-band"] {
		limit := gitMaxPacket
		if !v2 && !features.has["side-band-64k"] {
			limit = 1000
		}
		b.Write(gitSideband(pack, limit))
		b.WriteString("0000")
	} else {
		b.Write(pack)
	}
	return b.Bytes()
}
func (s *GitStore) upload(body []byte, v2 bool) []byte {
	c := gitCursorFor(body)
	if !v2 {
		wants := gitSection(c, 0)
		if len(wants) == 0 {
			return []byte("0000")
		}
		neg := []string{}
		for c.p < c.end {
			line, control := gitPacket(c)
			if control == 0 {
				break
			}
			gitAssert(control == -1, "invalid negotiation")
			neg = append(neg, line)
		}
		gitAssert(c.p == c.end, "trailing request bytes")
		return s.fetch(append(wants, neg...), false)
	}
	caps := gitSection(c, 1)
	args := gitSection(c, 0)
	gitAssert(c.p == c.end, "trailing request bytes")
	for _, line := range caps {
		gitAssert(!strings.HasPrefix(line, "object-format=") || line == "object-format=sha1", "unsupported object format")
	}
	gitAssert(len(caps) > 0, "unsupported upload-pack command")
	switch caps[0] {
	case "command=ls-refs":
		return s.lsRefs(args)
	case "command=fetch":
		return s.fetch(args, true)
	}
	panic("unsupported upload-pack command")
}
func gitAttempt(f func()) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	f()
	return true
}
func (s *GitStore) receive(body []byte) []byte {
	c := gitCursorFor(body)
	raw := gitSection(c, 0)
	if len(raw) == 0 {
		return []byte{}
	}
	first := strings.Split(raw[0], "\x00")
	gitAssert(len(first) <= 2, "invalid receive-pack capabilities")
	raw[0] = first[0]
	cap := ""
	if len(first) > 1 {
		cap = first[1]
	}
	caps := gitNewSet(strings.Split(cap, " ")...)
	type command struct{ old, next, ref string }
	commands := []command{}
	for _, line := range raw {
		p := strings.Split(line, " ")
		gitAssert(len(p) == 3 && gitOID.MatchString(p[0]) && gitOID.MatchString(p[1]) && p[2] != "" && !gitCommandRef.MatchString(p[2]), "invalid receive-pack command")
		commands = append(commands, command{p[0], p[1], p[2]})
	}
	incoming := map[string]*gitStoredObject{}
	unpackOK := gitAttempt(func() {
		if c.p < c.end {
			incoming = gitUnpack(body[c.p:], s.lookup)
		} else {
			for _, cmd := range commands {
				gitAssert(cmd.next == gitZero, "missing pack")
			}
		}
	})
	lookup := func(id string) *gitStoredObject {
		if o := s.lookup(id); o != nil {
			return o
		}
		return incoming[id]
	}
	var report bytes.Buffer
	if unpackOK {
		report.Write(gitPktText("unpack ok\n"))
	} else {
		report.Write(gitPktText("unpack invalid pack\n"))
	}
	// Non-atomic pushes publish each successful closure independently. Atomic
	// pushes validate against a private ref view, then publish only on success.
	atomic := caps.has["atomic"]
	refs := s
	if atomic && unpackOK {
		refs = &GitStore{Refs: maps.Clone(s.Refs), RefOrder: slices.Clone(s.RefOrder)}
	}
	staged := map[string]bool{}
	publish := func(id string) {
		if o := incoming[id]; o != nil && s.objects[id] == nil {
			// Keep only the compressed member, not the whole HTTP request.
			o.compressed = bytes.Clone(o.compressed)
			s.objects[id] = o
		}
		s.connected[id] = true
	}
	seen := map[string]bool{}
	reasons := make([]string, len(commands))
	failed := false
	for i, cmd := range commands {
		reason := ""
		if !unpackOK {
			reason = "unpacker error"
		}
		if reason == "" && !gitValidRef(cmd.ref) {
			reason = "invalid ref name"
		}
		if reason == "" && seen[cmd.ref] {
			reason = "duplicate command"
		}
		seen[cmd.ref] = true
		old := refs.Refs[cmd.ref]
		if old == "" {
			old = gitZero
		}
		if reason == "" && cmd.old != old {
			reason = "stale ref"
		}
		if reason == "" && cmd.next == gitZero && cmd.old == gitZero {
			reason = "no such ref"
		}
		if reason == "" && cmd.next != gitZero {
			if strings.HasPrefix(cmd.ref, "refs/heads/") {
				o := lookup(cmd.next)
				if o == nil || o.Type != "commit" {
					reason = "branch tip must be a commit"
				}
			}
			if reason == "" {
				for existing := range refs.Refs {
					if strings.HasPrefix(cmd.ref, existing+"/") || strings.HasPrefix(existing, cmd.ref+"/") {
						reason = "ref namespace conflict"
						break
					}
				}
			}
			if reason == "" {
				valid := gitAttempt(func() {
					closure := gitWalk([]string{cmd.next}, lookup, nil, s.connected)
					if cmd.old != gitZero && cmd.old != cmd.next && !gitAncestor(cmd.old, cmd.next, lookup) {
						if strings.HasPrefix(cmd.ref, "refs/tags/") {
							reason = "tag already exists"
						} else {
							reason = "non-fast-forward"
						}
					}
					if reason == "" {
						for _, id := range closure.ids {
							if atomic {
								staged[id] = true
							} else {
								publish(id)
							}
						}
						refs.SetRef(cmd.ref, cmd.next)
					}
				})
				if !valid {
					reason = "invalid object graph"
				}
			}
		} else if reason == "" {
			refs.DeleteRef(cmd.ref)
		}
		reasons[i] = reason
		failed = failed || reason != ""
	}
	if atomic && !failed {
		for id := range staged {
			publish(id)
		}
		s.Refs, s.RefOrder = refs.Refs, refs.RefOrder
	}
	for i, cmd := range commands {
		reason := reasons[i]
		if atomic && failed && reason == "" {
			reason = "atomic push failure"
		}
		if reason != "" {
			report.Write(gitPktText("ng " + cmd.ref + " " + reason + "\n"))
		} else {
			report.Write(gitPktText("ok " + cmd.ref + "\n"))
		}
	}
	if !caps.has["report-status"] && !caps.has["report-status-v2"] {
		if caps.has["side-band-64k"] {
			return []byte("0000")
		}
		return []byte{}
	}
	report.WriteString("0000")
	if caps.has["side-band-64k"] {
		return append(gitSideband(report.Bytes(), gitMaxPacket), []byte("0000")...)
	}
	return report.Bytes()
}
func gitResponse(code int, typ string, body []byte) wireResponse {
	return wireResponse{code: code, typ: typ, body: body, git: true}
}
func gitError(code int, msg string) wireResponse {
	return gitResponse(code, "application/json", append(appendJSONString([]byte(`{"message":`), msg), '}'))
}
func (s *GitStore) rawHTTP(method, protocol []byte, u *url.URL, service string, body []byte) (x wireResponse) {
	defer func() {
		if recover() != nil {
			x = gitError(400, "Malformed Git request")
		}
	}()
	v2 := gitProtocolV2.Match(protocol)
	if service == "info/refs" {
		if !bytes.Equal(method, []byte("GET")) {
			return gitError(405, "Method Not Allowed")
		}
		target := u.Query().Get("service")
		if target != "git-upload-pack" && target != "git-receive-pack" {
			return gitError(403, "Unsupported Git service")
		}
		return gitResponse(200, "application/x-"+target+"-advertisement", s.advertise(target, v2))

	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return gitError(404, "Not Found")
	}
	if !bytes.Equal(method, []byte("POST")) {
		return gitError(405, "Method Not Allowed")
	}
	if len(body) > gitMaxExpandedPack {
		return gitError(413, "Git request too large")
	}
	var result []byte
	if service == "git-upload-pack" {
		return s.uploadResponse(body, v2)
	} else {
		result = s.receive(body)
	}
	return gitResponse(200, "application/x-"+service+"-result", result)
}
