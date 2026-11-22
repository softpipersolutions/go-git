// Command gen builds three-way merge scenarios as git repositories.
//
// It is the input generator described in testdata/merge/SPEC.md. Every
// scenario is a repository with two branches, A and B, built through
// `git fast-import` with fixed identities, fixed dates and a fixed
// configuration, so the same seed gives the same commit ids on every machine.
//
//	go run ./testdata/merge/gen -out DIR -seed 1 -count 200
//	go run ./testdata/merge/gen -out DIR -case all
//
// For each scenario it writes DIR/<name> and prints one manifest line:
//
//	<name> <family> <path>
//
// The generator uses only the Go standard library and the git binary named
// by $GIT (default "git"). It never reads the user's git configuration.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Deterministic random numbers (splitmix64), independent of Go's math/rand.

type rng struct{ s uint64 }

func newRNG(seed uint64) *rng { return &rng{s: seed*0x9E3779B97F4A7C15 + 0x2545F4914F6CDD1D} }

func (r *rng) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// intn returns a number in [0, n).
func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// rangeIn returns a number in [lo, hi].
func (r *rng) rangeIn(lo, hi int) int { return lo + r.intn(hi-lo+1) }

// chance reports true with probability pct/100.
func (r *rng) chance(pct int) bool { return r.intn(100) < pct }

func pick[T any](r *rng, xs []T) T { return xs[r.intn(len(xs))] }

// ---------------------------------------------------------------------------
// Trees.

const (
	modeFile = "100644"
	modeExec = "100755"
)

type entry struct {
	mode string
	data string
}

type tree map[string]entry

func (t tree) clone() tree {
	c := make(tree, len(t))
	for k, v := range t {
		c[k] = v
	}
	return c
}

func (t tree) paths() []string {
	ps := make([]string, 0, len(t))
	for p := range t {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

// fits reports whether p can be added without a file/directory clash.
func (t tree) fits(p string) bool {
	if _, ok := t[p]; ok {
		return false
	}
	for q := range t {
		if strings.HasPrefix(q, p+"/") || strings.HasPrefix(p, q+"/") {
			return false
		}
	}
	return true
}

func (t tree) dirs() []string {
	set := map[string]bool{}
	for p := range t {
		for i := 0; i < len(p); i++ {
			if p[i] == '/' {
				set[p[:i]] = true
			}
		}
	}
	ds := make([]string, 0, len(set))
	for d := range set {
		ds = append(ds, d)
	}
	sort.Strings(ds)
	return ds
}

// ---------------------------------------------------------------------------
// File contents: small Go-like and prose-like text with many repeated lines
// ("}", blank lines, "return nil"), so that line alignment matters.

var stmts = []string{
	"\treturn nil",
	"\treturn err",
	"\tif err != nil {",
	"\t\treturn err",
	"\t}",
	"\tdefer f.Close()",
	"\tn++",
	"\tbuf.Reset()",
	"\tmu.Lock()",
	"\tmu.Unlock()",
	"\tfor _, v := range items {",
	"\t\ttotal += v",
	"\tctx := context.Background()",
	"\tlog.Println(\"done\")",
	"",
}

var words = []string{"alpha", "beta", "gamma", "delta", "omega", "kappa", "sigma", "theta", "lambda", "zeta"}

func freshLine(r *rng) string {
	switch r.intn(6) {
	case 0:
		return fmt.Sprintf("\tx%d := %d", r.intn(40), r.intn(1000))
	case 1:
		return fmt.Sprintf("\tlog.Printf(\"%s %%d\", %d)", pick(r, words), r.intn(100))
	case 2:
		return fmt.Sprintf("\t// %s %s %d", pick(r, words), pick(r, words), r.intn(100))
	case 3:
		return fmt.Sprintf("\tcfg.%s = %d", strings.Title(pick(r, words)), r.intn(500))
	case 4:
		return pick(r, stmts)
	default:
		return fmt.Sprintf("\tcall%d(%s)", r.intn(30), pick(r, words))
	}
}

var shellLines = []string{
	"set -e",
	"fi",
	"done",
	"",
	"go build ./...",
	"go vet ./...",
	"cd \"$ROOT\"",
	"exit 1",
}

// genShell returns a small shell script. Scripts are the files that carry the
// executable bit.
func genShell(r *rng, tag string) []string {
	ls := []string{"#!/bin/sh", "# " + tag, "set -e", ""}
	for i := r.rangeIn(3, 14); i > 0; i-- {
		switch r.intn(5) {
		case 0:
			ls = append(ls, fmt.Sprintf("if [ -n \"$%s\" ]; then", strings.ToUpper(pick(r, words))), fmt.Sprintf("\techo %s %d", pick(r, words), r.intn(100)), "fi")
		case 1:
			ls = append(ls, fmt.Sprintf("for f in %s/*; do", pick(r, words)), "\tgo test \"$f\"", "done")
		case 2:
			ls = append(ls, fmt.Sprintf("%s=%d", strings.ToUpper(pick(r, words)), r.intn(500)))
		default:
			ls = append(ls, pick(r, shellLines))
		}
	}
	return ls
}

func genFile(r *rng, path, tag string) entry {
	if strings.HasSuffix(path, ".sh") {
		mode := modeExec
		if r.chance(15) {
			mode = modeFile
		}
		return entry{mode, join(genShell(r, tag))}
	}
	mode := modeFile
	if r.chance(5) {
		mode = modeExec
	}
	return entry{mode, join(genContent(r, tag))}
}

func genContent(r *rng, tag string) []string {
	var ls []string
	if r.chance(30) {
		// prose-like file
		n := r.rangeIn(4, 24)
		for i := 0; i < n; i++ {
			if r.chance(20) {
				ls = append(ls, "")
				continue
			}
			ls = append(ls, fmt.Sprintf("%s %s %s line %d of %s.", strings.Title(pick(r, words)), pick(r, words), pick(r, words), i, tag))
		}
		return ls
	}
	blocks := r.rangeIn(2, 7)
	for b := 0; b < blocks; b++ {
		ls = append(ls, block(r, fmt.Sprintf("%s%d_%s", pick(r, words), b, tag), r.rangeIn(1, 8))...)
	}
	return ls
}

// block returns one Go-like function. Most of its lines also occur in other
// blocks, which gives line diffs several equally good alignments to choose
// between.
func block(r *rng, name string, k int) []string {
	ls := []string{fmt.Sprintf("func %s() error {", name)}
	for i := 0; i < k; i++ {
		if r.chance(55) {
			ls = append(ls, pick(r, stmts))
		} else {
			ls = append(ls, freshLine(r))
		}
	}
	return append(ls, "	return nil", "}", "")
}

func join(ls []string) string {
	if len(ls) == 0 {
		return ""
	}
	return strings.Join(ls, "\n") + "\n"
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// editLines applies 1..hunks random hunks. Hot spots, when given, attract
// hunks so that both sides of a merge touch the same region.
func editLines(r *rng, ls []string, hunks int, hot []int) []string {
	out := append([]string(nil), ls...)
	n := r.rangeIn(1, hunks)
	for h := 0; h < n; h++ {
		pos := r.intn(len(out) + 1)
		if len(hot) > 0 && r.chance(60) {
			pos = hot[r.intn(len(hot))]
			pos += r.rangeIn(-1, 1)
			if pos < 0 {
				pos = 0
			}
			if pos > len(out) {
				pos = len(out)
			}
		}
		var add []string
		del := 0
		switch k := r.intn(20); {
		case k < 10: // replace a few lines
			del = r.rangeIn(0, 3)
			for i := r.rangeIn(0, 3); i > 0; i-- {
				add = append(add, freshLine(r))
			}
			if del == 0 && len(add) == 0 {
				add = append(add, freshLine(r))
			}
		case k < 14: // repeat the lines just above, sometimes with one changed
			n := r.rangeIn(2, 5)
			if pos < n {
				n = pos
			}
			add = append(add, out[pos-n:pos]...)
			if len(add) > 0 && r.chance(50) {
				add[r.intn(len(add))] = freshLine(r)
			}
			if len(add) == 0 {
				add = append(add, freshLine(r))
			}
		case k < 17: // a new function made of common lines
			add = block(r, fmt.Sprintf("added%d", r.intn(1000)), r.rangeIn(1, 4))
		default: // delete a run of lines
			del = r.rangeIn(2, 6)
		}
		if pos+del > len(out) {
			del = len(out) - pos
		}
		if del == 0 && len(add) == 0 {
			add = append(add, freshLine(r))
		}
		tail := append([]string(nil), out[pos+del:]...)
		out = append(append(out[:pos], add...), tail...)
	}
	return out
}

func freshLines(r *rng, n int) []string {
	ls := make([]string, n)
	for i := range ls {
		ls[i] = freshLine(r)
	}
	return ls
}

// regionEdits returns two versions of ls in which both branches worked on the
// same few lines: a run of nearby changes, some made differently on the two
// sides, some identical on both (a helper or fix both branches picked up) and
// some on one side only. Differing changes sit one to three lines apart, and
// identical ones sit right next to them.
func regionEdits(r *rng, ls []string) (a, b []string) {
	pos := r.intn(len(ls) + 1)
	if len(ls) > 6 && pos > len(ls)-4 {
		pos = r.intn(len(ls) - 3)
	}
	a = append([]string(nil), ls[:pos]...)
	b = append([]string(nil), ls[:pos]...)
	// Each plan is a run of slots: 0 differing change, 1 identical change,
	// 2 one side only.
	plan := pick(r, [][]int{{0, 0}, {0, 0}, {0, 1, 0}, {0, 1, 0}, {0, 0, 0}, {0, 1, 0, 0}, {0, 2, 0}, {0, 0, 1, 0}, {1, 0, 1, 0}})
	prev := -1
	for s, kind := range plan {
		gap := 0
		switch {
		case s == 0:
		case kind == 0 && prev == 0:
			gap = r.rangeIn(1, 3)
		case kind == 2:
			gap = r.rangeIn(0, 1)
		}
		if pos+gap > len(ls) {
			gap = len(ls) - pos
		}
		a = append(a, ls[pos:pos+gap]...)
		b = append(b, ls[pos:pos+gap]...)
		pos += gap
		del := r.rangeIn(0, 2)
		if kind == 1 {
			del = r.rangeIn(0, 1)
		}
		if pos+del > len(ls) {
			del = len(ls) - pos
		}
		old := ls[pos : pos+del]
		switch kind {
		case 0: // both sides changed these lines, differently
			a = append(a, freshLines(r, r.rangeIn(1, 2))...)
			b = append(b, freshLines(r, r.rangeIn(1, 2))...)
		case 1: // the same change on both sides
			var add []string
			if r.chance(80) {
				add = block(r, fmt.Sprintf("shared%d", r.intn(1000)), r.rangeIn(1, 4))
			} else {
				add = freshLines(r, r.rangeIn(1, 3))
			}
			a = append(a, add...)
			b = append(b, add...)
		default: // one side only
			add := freshLines(r, r.rangeIn(1, 2))
			if r.chance(50) {
				a = append(a, add...)
				b = append(b, old...)
			} else {
				a = append(a, old...)
				b = append(b, add...)
			}
		}
		pos += del
		prev = kind
	}
	a = append(a, ls[pos:]...)
	b = append(b, ls[pos:]...)
	return a, b
}

// ---------------------------------------------------------------------------
// Paths.

var dirPool = []string{"", "", "src", "src/util", "lib", "docs", "cmd/tool", "pkg/core"}
var basePool = []string{"main.go", "util.go", "config.go", "server.go", "client.go", "store.go", "README.txt", "notes.txt", "api.go", "cache.go", "parse.go", "types.go", "index.go", "log.go", "build.sh", "release.sh", "check.sh"}

func joinPath(dir, base string) string {
	if dir == "" {
		return base
	}
	return dir + "/" + base
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

func baseOf(p string) string { return p[strings.LastIndexByte(p, '/')+1:] }

// newPath finds an unused path. kind: 0 any, 1 same dir as near, 2 other dir
// with the same basename as near.
func newPath(r *rng, t tree, near string, kind int) string {
	for try := 0; try < 40; try++ {
		var p string
		switch kind {
		case 1:
			p = joinPath(dirOf(near), fmt.Sprintf("%s_%s", pick(r, words), pick(r, basePool)))
		case 2:
			p = joinPath(pick(r, dirPool), baseOf(near))
		default:
			b := pick(r, basePool)
			if r.chance(40) {
				b = pick(r, words) + "_" + b
			}
			p = joinPath(pick(r, dirPool), b)
		}
		if p != near && t.fits(p) {
			return p
		}
	}
	return ""
}

func baseTree(r *rng, tag string) tree {
	t := tree{}
	n := r.rangeIn(3, 9)
	for len(t) < n {
		p := newPath(r, t, "", 0)
		if p == "" {
			break
		}
		t[p] = genFile(r, p, fmt.Sprintf("%s%d", tag, len(t)))
	}
	// Occasionally two files share most of their content, so that rename
	// detection has close candidates to choose between.
	if r.chance(30) && len(t) >= 2 {
		ps := t.paths()
		src := ps[r.intn(len(ps))]
		if p := newPath(r, t, src, pick(r, []int{1, 2})); p != "" {
			ls := split(t[src].data)
			if len(ls) > 2 {
				ls = editLines(r, ls, 1, nil)
			}
			t[p] = entry{t[src].mode, join(ls)}
		}
	}
	return t
}

// ---------------------------------------------------------------------------
// Operations applied to one side.

type side struct {
	r   *rng
	t   tree
	hot map[string][]int
	tag string
	n   int
}

func (s *side) files() []string { return s.t.paths() }

func (s *side) choose(shared []string) string {
	if len(shared) > 0 && s.r.chance(60) {
		p := pick(s.r, shared)
		if _, ok := s.t[p]; ok {
			return p
		}
	}
	fs := s.files()
	if len(fs) == 0 {
		return ""
	}
	return pick(s.r, fs)
}

func (s *side) fresh() string {
	s.n++
	return fmt.Sprintf("%s%d", s.tag, s.n)
}

func (s *side) edit(p string) bool {
	e, ok := s.t[p]
	if !ok {
		return false
	}
	ls := split(e.data)
	ls = editLines(s.r, ls, 3, s.hot[p])
	e.data = join(ls)
	s.t[p] = e
	return true
}

func (s *side) add(p string) bool {
	if p == "" || !s.t.fits(p) {
		return false
	}
	s.t[p] = genFile(s.r, p, s.fresh())
	return true
}

func (s *side) del(p string) bool {
	if _, ok := s.t[p]; !ok {
		return false
	}
	delete(s.t, p)
	return true
}

// rename moves p and, with partialEdit, edits it a little on the way.
func (s *side) rename(p string, to string, partialEdit bool) bool {
	e, ok := s.t[p]
	if !ok {
		return false
	}
	delete(s.t, p)
	if to == "" || !s.t.fits(to) {
		s.t[p] = e
		return false
	}
	if partialEdit {
		ls := split(e.data)
		if len(ls) > 3 {
			ls = editLines(s.r, ls, 2, nil)
			e.data = join(ls)
		}
	}
	s.t[to] = e
	return true
}

func (s *side) chmod(p string) bool {
	e, ok := s.t[p]
	if !ok {
		return false
	}
	if e.mode == modeExec {
		e.mode = modeFile
	} else {
		e.mode = modeExec
	}
	s.t[p] = e
	return true
}

// toDir replaces file p with a directory p/ holding new files; sometimes the
// old content moves inside it.
func (s *side) toDir(p string) bool {
	e, ok := s.t[p]
	if !ok {
		return false
	}
	delete(s.t, p)
	n := s.r.rangeIn(1, 2)
	for i := 0; i < n; i++ {
		q := p + "/" + pick(s.r, basePool)
		if !s.t.fits(q) {
			continue
		}
		if i == 0 && s.r.chance(40) {
			s.t[q] = e
		} else {
			s.t[q] = entry{modeFile, join(genContent(s.r, s.fresh()))}
		}
	}
	return true
}

// fromDir replaces directory d with a file d.
func (s *side) fromDir(d string) bool {
	found := false
	for p := range s.t {
		if strings.HasPrefix(p, d+"/") {
			delete(s.t, p)
			found = true
		}
	}
	if !found {
		return false
	}
	s.t[d] = entry{modeFile, join(genContent(s.r, s.fresh()))}
	return true
}

func (s *side) op(kind string, shared []string) {
	r := s.r
	switch kind {
	case "edit":
		s.edit(s.choose(shared))
	case "add":
		s.add(newPath(r, s.t, "", 0))
	case "del":
		s.del(s.choose(shared))
	case "rename":
		p := s.choose(shared)
		s.rename(p, newPath(r, s.t, p, r.intn(3)), r.chance(70))
	case "chmod":
		s.chmod(s.choose(shared))
	case "todir":
		s.toDir(s.choose(shared))
	case "fromdir":
		ds := s.t.dirs()
		if len(ds) > 0 {
			s.fromDir(pick(r, ds))
		}
	}
}

// ---------------------------------------------------------------------------
// Families.

var families = []string{"edit", "adddel", "rename", "mode", "filedir", "crisscross", "mixed"}

// familyWeight is how many seeds in a hundred pick each family. Most seeds
// are criss-cross histories, overlapping edits and renames.
var familyWeight = map[string]int{"crisscross": 36, "edit": 27, "rename": 22, "mode": 4, "adddel": 4, "filedir": 3, "mixed": 4}

func pickFamily(r *rng) string {
	n := r.intn(100)
	for _, f := range families {
		if n < familyWeight[f] {
			return f
		}
		n -= familyWeight[f]
	}
	return families[0]
}

// regionChance is the percentage of seeds in each family where both sides
// also work on the same lines of one file.
var regionChance = map[string]int{"edit": 90, "adddel": 80, "rename": 55, "mode": 80, "filedir": 80, "mixed": 80}

var familyOps = map[string][]string{
	"edit":    {"edit", "edit", "edit", "edit", "add"},
	"adddel":  {"add", "del", "edit", "edit", "del"},
	"rename":  {"rename", "rename", "edit", "edit", "del", "add"},
	"mode":    {"chmod", "chmod", "edit", "edit", "rename"},
	"filedir": {"todir", "fromdir", "edit", "add", "del"},
	"mixed":   {"edit", "edit", "add", "del", "rename", "rename", "chmod", "todir", "fromdir"},
}

type commit struct {
	mark    int
	t       tree
	parents []int
	msg     string
}

type scenario struct {
	name    string
	family  string
	commits []commit
	refs    map[string]int // branch -> mark
	bare    bool
	head    string // checked-out branch for non-bare repositories
}

func (sc *scenario) add(t tree, msg string, parents ...int) int {
	m := len(sc.commits) + 1
	sc.commits = append(sc.commits, commit{mark: m, t: t, parents: parents, msg: msg})
	return m
}

// pairOps adds coordinated changes so that both sides touch the same path in
// ways that interact (add/add, modify/delete, rename/rename and so on).
func pairOps(r *rng, family string, a, b *side, base tree) {
	ps := base.paths()
	if len(ps) == 0 {
		return
	}
	p := pick(r, ps)
	switch family {
	case "edit":
		a.edit(p)
		b.edit(p)
	case "adddel":
		switch r.intn(3) {
		case 0: // modify/delete
			a.edit(p)
			b.del(p)
		case 1: // add/add, same path
			q := newPath(r, base, "", 0)
			if q == "" {
				return
			}
			a.add(q)
			if r.chance(30) {
				b.t[q] = a.t[q]
			} else if r.chance(70) {
				// both started from the same draft and changed it
				e := a.t[q]
				la, lb := regionEdits(r, split(e.data))
				e.data = join(la)
				a.t[q] = e
				e.data = join(lb)
				b.t[q] = e
			} else {
				b.add(q)
			}
		default: // delete/delete
			a.del(p)
			b.del(p)
		}
	case "rename":
		switch r.intn(9) {
		case 0: // rename vs edit
			a.rename(p, newPath(r, a.t, p, r.intn(3)), r.chance(60))
			b.edit(p)
		case 1: // rename/rename to different names
			a.rename(p, newPath(r, a.t, p, r.intn(3)), r.chance(50))
			b.rename(p, newPath(r, b.t, p, r.intn(3)), r.chance(50))
		case 2: // rename/delete
			a.rename(p, newPath(r, a.t, p, r.intn(3)), r.chance(50))
			b.del(p)
		case 3: // two files renamed onto one name
			if len(ps) < 2 {
				return
			}
			q := pick(r, ps)
			if q == p {
				return
			}
			to := newPath(r, base, "", 0)
			if to == "" {
				return
			}
			a.rename(p, to, r.chance(50))
			b.rename(q, to, r.chance(50))
		case 4: // rename vs add at the target
			to := newPath(r, base, p, r.intn(3))
			if to == "" {
				return
			}
			a.rename(p, to, r.chance(60))
			b.add(to)
		case 5, 6: // a script renamed and edited, edited on the other side too
			for _, q := range ps {
				if base[q].mode == modeExec {
					p = q
					break
				}
			}
			to := newPath(r, a.t, p, r.intn(3))
			a.rename(p, to, true)
			if r.chance(40) {
				a.chmod(to)
			}
			b.edit(p)
		case 7: // rename that also flips the executable bit
			to := newPath(r, a.t, p, r.intn(3))
			a.rename(p, to, r.chance(70))
			a.chmod(to)
			b.edit(p)
		default: // rename with a heavier edit on the way
			to := newPath(r, a.t, p, r.intn(3))
			if a.rename(p, to, false) {
				e := a.t[to]
				if ls := split(e.data); len(ls) > 3 {
					e.data = join(editLines(r, ls, 4, nil))
					a.t[to] = e
				}
			}
			b.edit(p)
		}
	case "mode":
		switch r.intn(4) {
		case 0:
			a.chmod(p)
			b.edit(p)
		case 1:
			a.chmod(p)
			b.chmod(p)
			b.edit(p)
		default: // moved and made executable (or not) on one side, edited on the other
			to := newPath(r, a.t, p, r.intn(3))
			if a.rename(p, to, r.chance(60)) {
				a.chmod(to)
			}
			b.edit(p)
		}
	case "filedir":
		if r.chance(60) {
			a.toDir(p)
			if r.chance(70) {
				b.edit(p)
			}
		} else if ds := base.dirs(); len(ds) > 0 {
			d := pick(r, ds)
			a.fromDir(d)
			if r.chance(50) {
				b.add(d + "/" + pick(r, basePool))
			} else {
				for _, f := range b.files() {
					if strings.HasPrefix(f, d+"/") {
						b.edit(f)
						break
					}
				}
			}
		}
	}
}

func hotSpots(r *rng, t tree) map[string][]int {
	h := map[string][]int{}
	for _, p := range t.paths() {
		n := len(split(t[p].data))
		k := r.rangeIn(1, 2)
		for i := 0; i < k; i++ {
			h[p] = append(h[p], r.intn(n+1))
		}
	}
	return h
}

// regionPair makes both sides work on the same lines of one file that both
// still hold with equal content.
func regionPair(r *rng, a, b *side) bool {
	var cands []string
	for _, p := range a.t.paths() {
		if e, ok := b.t[p]; ok && e == a.t[p] && len(split(e.data)) >= 3 {
			cands = append(cands, p)
		}
	}
	if len(cands) == 0 {
		return false
	}
	p := pick(r, cands)
	ea, eb := a.t[p], b.t[p]
	la, lb := regionEdits(r, split(ea.data))
	ea.data, eb.data = join(la), join(lb)
	a.t[p], b.t[p] = ea, eb
	return true
}

// regionTrees returns two copies of t in which both sides worked on the same
// lines of one file, the start of two branches that touch the same function.
func regionTrees(r *rng, t tree) (tree, tree) {
	a, b := t.clone(), t.clone()
	var cands []string
	for _, p := range t.paths() {
		if len(split(t[p].data)) >= 6 {
			cands = append(cands, p)
		}
	}
	if len(cands) == 0 {
		return a, b
	}
	p := pick(r, cands)
	la, lb := regionEdits(r, split(t[p].data))
	ea, eb := t[p], t[p]
	ea.data, eb.data = join(la), join(lb)
	a[p], b[p] = ea, eb
	return a, b
}

// splitPair makes the two sides edit the same file far enough apart that a
// merge of the two takes both edits without a conflict.
func splitPair(r *rng, a, b *side) bool {
	var cands []string
	for _, p := range a.t.paths() {
		if e, ok := b.t[p]; ok && e == a.t[p] && len(split(e.data)) >= 10 {
			cands = append(cands, p)
		}
	}
	if len(cands) == 0 {
		return false
	}
	p := pick(r, cands)
	ls := split(a.t[p].data)
	n := len(ls)
	i := r.intn(n/2 - 2)
	j := n/2 + 1 + r.intn(n-n/2-1)
	la := append(append(append([]string(nil), ls[:i]...), freshLines(r, r.rangeIn(1, 2))...), ls[i+1:]...)
	lb := append(append(append([]string(nil), ls[:j]...), freshLines(r, r.rangeIn(1, 2))...), ls[j+1:]...)
	ea, eb := a.t[p], b.t[p]
	ea.data, eb.data = join(la), join(lb)
	a.t[p], b.t[p] = ea, eb
	return true
}

// evolve applies the family's operations to a copy of t.
func evolve(r *rng, family string, t tree, hot map[string][]int, tag string, shared []string, nops int) *side {
	s := &side{r: r, t: t.clone(), hot: hot, tag: tag}
	ops := familyOps[family]
	for i := 0; i < nops; i++ {
		s.op(pick(r, ops), shared)
	}
	return s
}

// resolve builds the tree of a merge commit by taking, file by file, the side
// that changed a path, and the preferred side where both did.
func resolve(base, pref, other tree) tree {
	out := tree{}
	seen := map[string]bool{}
	var all []string
	for _, t := range []tree{base, pref, other} {
		for p := range t {
			if !seen[p] {
				seen[p] = true
				all = append(all, p)
			}
		}
	}
	sort.Strings(all)
	for _, p := range all {
		be, bok := base[p]
		pe, pok := pref[p]
		oe, ook := other[p]
		prefChanged := pok != bok || pe != be
		otherChanged := ook != bok || oe != be
		switch {
		case !otherChanged || prefChanged:
			if pok {
				out[p] = pe
			}
		default:
			if ook {
				out[p] = oe
			}
		}
	}
	// Drop entries that clash with a file/directory from the preferred side.
	for _, p := range out.paths() {
		if _, ok := out[p]; !ok {
			continue
		}
		for _, q := range out.paths() {
			if strings.HasPrefix(q, p+"/") {
				if _, inPref := pref[p]; inPref {
					delete(out, q)
				} else {
					delete(out, p)
					break
				}
			}
		}
	}
	return out
}

func seeded(seed uint64) *scenario {
	r := newRNG(seed)
	family := pickFamily(r)
	sc := &scenario{name: fmt.Sprintf("s%d", seed), family: family, refs: map[string]int{}, bare: true}
	base := baseTree(r, fmt.Sprintf("s%d", seed))
	o := sc.add(base, "base")

	if family == "crisscross" {
		inner := pick(r, []string{"edit", "edit", "rename", "rename", "mode", "adddel", "mixed"})
		hot := hotSpots(r, base)
		shared := base.paths()
		a1 := evolve(r, inner, base, hot, "a", shared, r.rangeIn(1, 3))
		b1 := evolve(r, inner, base, hot, "b", shared, r.rangeIn(1, 3))
		pairOps(r, inner, a1, b1, base)
		// Each merge commit below keeps its own side's version of a file that
		// both sides changed: either the same lines (the merge bases then
		// conflict) or lines far apart (they merge cleanly).
		if r.chance(55) || !splitPair(r, a1, b1) {
			regionPair(r, a1, b1)
		}
		ma1 := sc.add(a1.t, "A1", o)
		mb1 := sc.add(b1.t, "B1", o)
		// shape 0: two merge bases; 1: three; 2: two rounds of cross merges,
		// so the two merge bases have two merge bases of their own.
		shape := pick(r, []int{0, 0, 0, 1, 1, 2, 2})
		a2t := resolve(base, a1.t, b1.t)
		b2t := resolve(base, b1.t, a1.t)
		pa := []int{ma1, mb1}
		pb := []int{mb1, ma1}
		if shape == 1 {
			c1 := evolve(r, inner, base, hot, "c", shared, r.rangeIn(1, 2))
			regionPair(r, a1, c1)
			mc1 := sc.add(c1.t, "C1", o)
			a2t = resolve(base, a2t, c1.t)
			b2t = resolve(base, b2t, c1.t)
			pa = append(pa, mc1)
			pb = append(pb, mc1)
		}
		ma2 := sc.add(a2t, "A2 merge", pa...)
		mb2 := sc.add(b2t, "B2 merge", pb...)
		if shape == 2 {
			shared2 := intersect(a2t.paths(), b2t.paths())
			hot2 := hotSpots(r, a2t)
			a3 := evolve(r, inner, a2t, hot2, "a3", shared2, r.rangeIn(1, 2))
			b3 := evolve(r, inner, b2t, hot2, "b3", shared2, r.rangeIn(1, 2))
			regionPair(r, a3, b3)
			ma3 := sc.add(a3.t, "A3", ma2)
			mb3 := sc.add(b3.t, "B3", mb2)
			a4t := resolve(a2t, a3.t, b3.t)
			b4t := resolve(b2t, b3.t, a3.t)
			ma2 = sc.add(a4t, "A4 merge", ma3, mb3)
			mb2 = sc.add(b4t, "B4 merge", mb3, ma3)
			a2t, b2t = a4t, b4t
		}
		shared2 := intersect(a2t.paths(), b2t.paths())
		hot2 := hotSpots(r, a2t)
		a3 := evolve(r, inner, a2t, hot2, "a3", shared2, r.rangeIn(0, 2))
		b3 := evolve(r, inner, b2t, hot2, "b3", shared2, r.rangeIn(0, 2))
		sc.refs["A"] = sc.add(a3.t, "A tip", ma2)
		sc.refs["B"] = sc.add(b3.t, "B tip", mb2)
		return sc
	}

	hot := hotSpots(r, base)
	shared := base.paths()
	at, bt := base, base
	if r.chance(regionChance[family]) {
		at, bt = regionTrees(r, base)
	}
	a := evolve(r, family, at, hot, "a", shared, r.rangeIn(1, 4))
	b := evolve(r, family, bt, hot, "b", shared, r.rangeIn(1, 4))
	for i := r.rangeIn(1, 2); i > 0; i-- {
		pairOps(r, family, a, b, base)
	}
	if family != "edit" && family != "rename" {
		regionPair(r, a, b)
	}
	// A short history on each side, so merge-base search walks more than one
	// commit.
	ma := o
	mb := o
	if r.chance(40) {
		mid := evolve(r, "edit", base, hot, "am", nil, 1)
		ma = sc.add(mid.t, "A1", o)
		a.t = mergeOnto(base, mid.t, a.t)
	}
	if r.chance(40) {
		mid := evolve(r, "edit", base, hot, "bm", nil, 1)
		mb = sc.add(mid.t, "B1", o)
		b.t = mergeOnto(base, mid.t, b.t)
	}
	sc.refs["A"] = sc.add(a.t, "A", ma)
	sc.refs["B"] = sc.add(b.t, "B", mb)
	return sc
}

// mergeOnto keeps the intermediate commit's changes where the final side
// left a path untouched.
func mergeOnto(base, mid, final tree) tree { return resolve(base, final, mid) }

func intersect(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Hand-built cases.

func lines(n int, tag string) []string {
	var ls []string
	for i := 1; i <= n; i++ {
		ls = append(ls, fmt.Sprintf("%s line %d", tag, i))
	}
	return ls
}

func replaceAt(ls []string, i int, with ...string) []string {
	out := append([]string(nil), ls[:i]...)
	out = append(out, with...)
	return append(out, ls[i+1:]...)
}

func f(ls []string) entry { return entry{modeFile, join(ls)} }

type handCase struct {
	name string
	desc string
	make func() *scenario
}

func simple(name string, base, a, b tree) *scenario {
	sc := &scenario{name: name, family: "case", refs: map[string]int{}, bare: true}
	o := sc.add(base, "base")
	sc.refs["A"] = sc.add(a, "A", o)
	sc.refs["B"] = sc.add(b, "B", o)
	return sc
}

func handCases() []handCase {
	ten := lines(10, "body")
	return []handCase{
		{"ff-b-ahead", "B is A plus one commit (fast-forward)", func() *scenario {
			sc := &scenario{name: "ff-b-ahead", family: "case", refs: map[string]int{}, bare: true}
			o := sc.add(tree{"a.txt": f(ten)}, "base")
			sc.refs["A"] = o
			sc.refs["B"] = sc.add(tree{"a.txt": f(replaceAt(ten, 2, "B edit")), "new.txt": f(lines(2, "n"))}, "B", o)
			return sc
		}},
		{"ff-a-ahead", "A is B plus one commit (already up to date)", func() *scenario {
			sc := &scenario{name: "ff-a-ahead", family: "case", refs: map[string]int{}, bare: true}
			o := sc.add(tree{"a.txt": f(ten)}, "base")
			sc.refs["B"] = o
			sc.refs["A"] = sc.add(tree{"a.txt": f(replaceAt(ten, 7, "A edit"))}, "A", o)
			return sc
		}},
		{"clean-disjoint", "edits to different hunks of one file and to different files", func() *scenario {
			base := tree{"a.txt": f(ten), "b.txt": f(lines(4, "b"))}
			a := tree{"a.txt": f(replaceAt(ten, 1, "A edit")), "b.txt": f(lines(4, "b"))}
			b := tree{"a.txt": f(replaceAt(ten, 8, "B edit")), "b.txt": f(append(lines(4, "b"), "B tail"))}
			return simple("clean-disjoint", base, a, b)
		}},
		{"adjacent-hunks", "edits to neighbouring lines of one file", func() *scenario {
			base := tree{"a.txt": f(ten)}
			a := tree{"a.txt": f(replaceAt(ten, 4, "A edit"))}
			b := tree{"a.txt": f(replaceAt(ten, 5, "B edit"))}
			return simple("adjacent-hunks", base, a, b)
		}},
		{"same-change-both", "both sides make the identical change plus one each", func() *scenario {
			base := tree{"a.txt": f(ten)}
			both := replaceAt(ten, 2, "shared edit")
			a := tree{"a.txt": f(replaceAt(both, 7, "A only"))}
			b := tree{"a.txt": f(both)}
			return simple("same-change-both", base, a, b)
		}},
		{"rename-edit-vs-edit", "A renames with a small edit, B edits the old path", func() *scenario {
			base := tree{"src/old.go": f(lines(12, "code"))}
			a := tree{"pkg/new.go": f(replaceAt(lines(12, "code"), 0, "A first"))}
			b := tree{"src/old.go": f(replaceAt(lines(12, "code"), 11, "B last"))}
			return simple("rename-edit-vs-edit", base, a, b)
		}},
		{"rename-rename-1to2", "one file renamed differently on each side", func() *scenario {
			base := tree{"x.txt": f(ten)}
			a := tree{"ax.txt": f(ten)}
			b := tree{"bx.txt": f(ten)}
			return simple("rename-rename-1to2", base, a, b)
		}},
		{"rename-rename-2to1", "two files renamed onto the same path", func() *scenario {
			base := tree{"one.txt": f(lines(8, "one")), "two.txt": f(lines(8, "two"))}
			a := tree{"same.txt": f(lines(8, "one")), "two.txt": f(lines(8, "two"))}
			b := tree{"one.txt": f(lines(8, "one")), "same.txt": f(lines(8, "two"))}
			return simple("rename-rename-2to1", base, a, b)
		}},
		{"rename-delete", "A renames, B deletes", func() *scenario {
			base := tree{"keep.txt": f(lines(3, "k")), "gone.txt": f(ten)}
			a := tree{"keep.txt": f(lines(3, "k")), "moved.txt": f(replaceAt(ten, 3, "A edit"))}
			b := tree{"keep.txt": f(lines(3, "k"))}
			return simple("rename-delete", base, a, b)
		}},
		{"add-add-differ", "both add one path; the two versions differ in two places", func() *scenario {
			base := tree{"k.txt": f(lines(2, "k"))}
			n := lines(12, "n")
			a := tree{"k.txt": f(lines(2, "k")), "new.txt": f(replaceAt(replaceAt(n, 2, "A head"), 9, "A tail"))}
			b := tree{"k.txt": f(lines(2, "k")), "new.txt": f(replaceAt(replaceAt(n, 2, "B head"), 9, "B tail"))}
			return simple("add-add-differ", base, a, b)
		}},
		{"modify-delete", "A modifies, B deletes", func() *scenario {
			base := tree{"k.txt": f(lines(2, "k")), "m.txt": f(ten)}
			a := tree{"k.txt": f(lines(2, "k")), "m.txt": f(replaceAt(ten, 0, "A edit"))}
			b := tree{"k.txt": f(lines(2, "k"))}
			return simple("modify-delete", base, a, b)
		}},
		{"file-to-dir", "A turns a file into a directory, B edits the file", func() *scenario {
			base := tree{"node": f(ten)}
			a := tree{"node/child.txt": f(lines(3, "c"))}
			b := tree{"node": f(replaceAt(ten, 2, "B edit"))}
			return simple("file-to-dir", base, a, b)
		}},
		{"mode-and-edit", "A sets the executable bit, B edits", func() *scenario {
			base := tree{"run.sh": f(ten)}
			a := tree{"run.sh": entry{modeExec, join(ten)}}
			b := tree{"run.sh": f(replaceAt(ten, 9, "B edit"))}
			return simple("mode-and-edit", base, a, b)
		}},
		{"basename-move", "a file moves to another directory keeping its name among similar files", func() *scenario {
			body := lines(12, "shared")
			base := tree{"a/util.go": f(body), "b/helper.go": f(replaceAt(body, 0, "helper"))}
			a := tree{"c/util.go": f(replaceAt(body, 11, "A edit")), "b/helper.go": f(replaceAt(body, 0, "helper"))}
			b := tree{"a/util.go": f(replaceAt(body, 5, "B edit")), "b/helper.go": f(replaceAt(body, 0, "helper"))}
			return simple("basename-move", base, a, b)
		}},
		{"crisscross-conflict", "criss-cross history whose two merge bases conflict", func() *scenario {
			sc := &scenario{name: "crisscross-conflict", family: "case", refs: map[string]int{}, bare: true}
			o := sc.add(tree{"a.txt": f(ten)}, "base")
			a1 := sc.add(tree{"a.txt": f(replaceAt(ten, 4, "A1 edit"))}, "A1", o)
			b1 := sc.add(tree{"a.txt": f(replaceAt(ten, 4, "B1 edit"))}, "B1", o)
			a2 := sc.add(tree{"a.txt": f(replaceAt(ten, 4, "A1 edit"))}, "A2", a1, b1)
			b2 := sc.add(tree{"a.txt": f(replaceAt(ten, 4, "B1 edit"))}, "B2", b1, a1)
			sc.refs["A"] = sc.add(tree{"a.txt": f(replaceAt(replaceAt(ten, 4, "A1 edit"), 0, "A3 edit"))}, "A3", a2)
			sc.refs["B"] = sc.add(tree{"a.txt": f(replaceAt(replaceAt(ten, 4, "B1 edit"), 9, "B3 edit"))}, "B3", b2)
			return sc
		}},
		{"close-conflicts", "two conflicting edits two lines apart", func() *scenario {
			base := tree{"a.txt": f(lines(12, "body"))}
			a := tree{"a.txt": f(replaceAt(replaceAt(lines(12, "body"), 3, "A first"), 6, "A second"))}
			b := tree{"a.txt": f(replaceAt(replaceAt(lines(12, "body"), 3, "B first"), 6, "B second"))}
			return simple("close-conflicts", base, a, b)
		}},
		{"shared-block-split", "both insert the same helper between conflicting edits", func() *scenario {
			body := lines(12, "body")
			helper := []string{"func helper() error {", "\tmu.Lock()", "\tdefer mu.Unlock()", "\treturn flush(buf)", "}"}
			a := append(append(append(append([]string(nil), body[:4]...), "A edit"), helper...), append([]string{"A after"}, body[6:]...)...)
			b := append(append(append(append([]string(nil), body[:4]...), "B edit"), helper...), append([]string{"B after"}, body[6:]...)...)
			return simple("shared-block-split", tree{"a.go": f(body)}, tree{"a.go": f(a)}, tree{"a.go": f(b)})
		}},
		{"rename-script-delete", "A moves an executable script with an edit, B deletes it", func() *scenario {
			sh := append([]string{"#!/bin/sh", "set -e"}, lines(10, "step")...)
			base := tree{"keep.txt": f(lines(3, "k")), "tools/check.sh": entry{modeExec, join(sh)}}
			a := tree{"keep.txt": f(lines(3, "k")), "ci/check.sh": entry{modeExec, join(replaceAt(sh, 5, "echo moved"))}}
			b := tree{"keep.txt": f(lines(3, "k"))}
			return simple("rename-script-delete", base, a, b)
		}},
		{"rename-script-edit", "an executable script moves with an edit, the other side edits it", func() *scenario {
			sh := append([]string{"#!/bin/sh", "set -e"}, lines(10, "step")...)
			base := tree{"tools/build.sh": entry{modeExec, join(sh)}}
			a := tree{"scripts/build.sh": entry{modeExec, join(replaceAt(sh, 2, "echo moved"))}}
			b := tree{"tools/build.sh": entry{modeExec, join(replaceAt(sh, 10, "echo B"))}}
			return simple("rename-script-edit", base, a, b)
		}},
		{"rename-chmod-edit", "A moves a file and sets the executable bit, B edits it", func() *scenario {
			base := tree{"run.sh": f(ten)}
			a := tree{"bin/run.sh": entry{modeExec, join(ten)}}
			b := tree{"run.sh": f(replaceAt(ten, 9, "B edit"))}
			return simple("rename-chmod-edit", base, a, b)
		}},
		{"basename-pairing", "a moved file is closer in content to another deleted file than to its namesake", func() *scenario {
			conf := lines(12, "setting")
			other := replaceAt(conf, 0, "settings header")
			base := tree{"src/conf.go": f(conf), "lib/settings.go": f(other)}
			a := tree{"pkg/conf.go": f(replaceAt(other, 11, "A tail"))}
			b := tree{"src/conf.go": f(replaceAt(conf, 6, "B edit")), "lib/settings.go": f(other)}
			return simple("basename-pairing", base, a, b)
		}},
		{"crisscross-clean-base", "criss-cross whose merges each kept one side; the merged base is clean", func() *scenario {
			sc := &scenario{name: "crisscross-clean-base", family: "case", refs: map[string]int{}, bare: true}
			o := sc.add(tree{"a.txt": f(ten)}, "base")
			a1t := tree{"a.txt": f(replaceAt(ten, 2, "A1 edit"))}
			b1t := tree{"a.txt": f(replaceAt(ten, 8, "B1 edit"))}
			a1 := sc.add(a1t, "A1", o)
			b1 := sc.add(b1t, "B1", o)
			a2 := sc.add(a1t, "A2", a1, b1)
			b2 := sc.add(b1t, "B2", b1, a1)
			sc.refs["A"] = sc.add(tree{"a.txt": f(replaceAt(replaceAt(ten, 2, "A1 edit"), 0, "A tip"))}, "A3", a2)
			sc.refs["B"] = sc.add(tree{"a.txt": f(replaceAt(replaceAt(ten, 8, "B1 edit"), 5, "B tip"))}, "B3", b2)
			return sc
		}},
		{"crisscross-three-bases", "three branches merged both ways, so the final merge has three bases", func() *scenario {
			sc := &scenario{name: "crisscross-three-bases", family: "case", refs: map[string]int{}, bare: true}
			body := lines(12, "body")
			o := sc.add(tree{"a.txt": f(body)}, "base")
			a1t := tree{"a.txt": f(replaceAt(body, 1, "A1 edit"))}
			a1 := sc.add(a1t, "A1", o)
			b1t := tree{"a.txt": f(replaceAt(body, 6, "B1 edit"))}
			b1 := sc.add(b1t, "B1", o)
			c1 := sc.add(tree{"a.txt": f(replaceAt(body, 10, "C1 edit"))}, "C1", o)
			a2 := sc.add(a1t, "A2", a1, b1, c1)
			b2 := sc.add(b1t, "B2", b1, a1, c1)
			sc.refs["A"] = sc.add(tree{"a.txt": f(replaceAt(replaceAt(body, 1, "A1 edit"), 3, "A tip"))}, "A3", a2)
			sc.refs["B"] = sc.add(tree{"a.txt": f(replaceAt(replaceAt(body, 6, "B1 edit"), 8, "B tip"))}, "B3", b2)
			return sc
		}},
		{"crisscross-twice", "two rounds of cross merges; the merge bases have two merge bases themselves", func() *scenario {
			sc := &scenario{name: "crisscross-twice", family: "case", refs: map[string]int{}, bare: true}
			body := lines(14, "body")
			o := sc.add(tree{"a.txt": f(body)}, "base")
			a1v := replaceAt(body, 2, "A1 edit")
			b1v := replaceAt(body, 2, "B1 edit")
			a1 := sc.add(tree{"a.txt": f(a1v)}, "A1", o)
			b1 := sc.add(tree{"a.txt": f(b1v)}, "B1", o)
			a2 := sc.add(tree{"a.txt": f(a1v)}, "A2", a1, b1)
			b2 := sc.add(tree{"a.txt": f(b1v)}, "B2", b1, a1)
			a3v := replaceAt(a1v, 6, "A3 edit")
			b3v := replaceAt(b1v, 10, "B3 edit")
			a3 := sc.add(tree{"a.txt": f(a3v)}, "A3", a2)
			b3 := sc.add(tree{"a.txt": f(b3v)}, "B3", b2)
			a4 := sc.add(tree{"a.txt": f(a3v)}, "A4", a3, b3)
			b4 := sc.add(tree{"a.txt": f(b3v)}, "B4", b3, a3)
			sc.refs["A"] = sc.add(tree{"a.txt": f(replaceAt(a3v, 0, "A tip"))}, "A5", a4)
			sc.refs["B"] = sc.add(tree{"a.txt": f(replaceAt(b3v, 13, "B tip"))}, "B5", b4)
			return sc
		}},
		{"crisscross-rename", "criss-cross where one merge kept a rename and the other kept the old name", func() *scenario {
			sc := &scenario{name: "crisscross-rename", family: "case", refs: map[string]int{}, bare: true}
			body := lines(12, "body")
			o := sc.add(tree{"a.txt": f(body)}, "base")
			a1t := tree{"b.txt": f(body)}
			b1t := tree{"a.txt": f(replaceAt(body, 3, "B1 edit"))}
			a1 := sc.add(a1t, "A1", o)
			b1 := sc.add(b1t, "B1", o)
			a2 := sc.add(a1t, "A2", a1, b1)
			b2 := sc.add(b1t, "B2", b1, a1)
			sc.refs["A"] = sc.add(tree{"b.txt": f(replaceAt(body, 8, "A tip"))}, "A3", a2)
			sc.refs["B"] = sc.add(tree{"a.txt": f(replaceAt(replaceAt(body, 3, "B1 edit"), 0, "B tip"))}, "B3", b2)
			return sc
		}},
		{"crisscross-add-add", "criss-cross whose bases added one path with different content", func() *scenario {
			sc := &scenario{name: "crisscross-add-add", family: "case", refs: map[string]int{}, bare: true}
			o := sc.add(tree{"k.txt": f(ten)}, "base")
			a1t := tree{"k.txt": f(ten), "new.txt": f(append(lines(6, "n"), "A1 tail"))}
			b1t := tree{"k.txt": f(ten), "new.txt": f(append(lines(6, "n"), "B1 tail"))}
			a1 := sc.add(a1t, "A1", o)
			b1 := sc.add(b1t, "B1", o)
			a2 := sc.add(a1t, "A2", a1, b1)
			b2 := sc.add(b1t, "B2", b1, a1)
			sc.refs["A"] = sc.add(tree{"k.txt": f(replaceAt(ten, 1, "A tip")), "new.txt": f(append(lines(6, "n"), "A1 tail"))}, "A3", a2)
			sc.refs["B"] = sc.add(tree{"k.txt": f(replaceAt(ten, 8, "B tip")), "new.txt": f(append(lines(6, "n"), "B1 tail"))}, "B3", b2)
			return sc
		}},
		{"cherry-pick-clean", "non-bare repository on A; B's tip edits b.txt, A edited a.txt", func() *scenario {
			sc := &scenario{name: "cherry-pick-clean", family: "cherrypick", refs: map[string]int{}, head: "A"}
			o := sc.add(tree{"a.txt": f(ten), "b.txt": f(ten)}, "base")
			sc.refs["A"] = sc.add(tree{"a.txt": f(replaceAt(ten, 3, "A keeps this")), "b.txt": f(ten)}, "A", o)
			b1 := sc.add(tree{"a.txt": f(ten), "b.txt": f(ten), "c.txt": f(lines(2, "c"))}, "B1", o)
			sc.refs["B"] = sc.add(tree{"a.txt": f(ten), "b.txt": f(replaceAt(ten, 6, "picked edit")), "c.txt": f(lines(2, "c"))}, "B pick me", b1)
			return sc
		}},
		{"cherry-pick-theirs", "non-bare repository on A; B's tip edits the same line A edited", func() *scenario {
			sc := &scenario{name: "cherry-pick-theirs", family: "cherrypick", refs: map[string]int{}, head: "A"}
			o := sc.add(tree{"a.txt": f(ten), "b.txt": f(ten)}, "base")
			sc.refs["A"] = sc.add(tree{"a.txt": f(replaceAt(replaceAt(ten, 2, "A version"), 8, "A only")), "b.txt": f(ten)}, "A", o)
			sc.refs["B"] = sc.add(tree{"a.txt": f(replaceAt(ten, 2, "B version")), "b.txt": f(append(lines(10, "body"), "B tail"))}, "B pick me", o)
			return sc
		}},
		{"cherry-pick-conflict", "non-bare repository on A; B's tip edits a file A deleted", func() *scenario {
			sc := &scenario{name: "cherry-pick-conflict", family: "cherrypick", refs: map[string]int{}, head: "A"}
			o := sc.add(tree{"a.txt": f(ten), "b.txt": f(ten)}, "base")
			sc.refs["A"] = sc.add(tree{"a.txt": f(ten)}, "A deletes b", o)
			sc.refs["B"] = sc.add(tree{"a.txt": f(ten), "b.txt": f(replaceAt(ten, 1, "B edit"))}, "B pick me", o)
			return sc
		}},
	}
}

// ---------------------------------------------------------------------------
// Writing repositories.

const repoConfig = `[merge]
	conflictStyle = merge
	renames = true
	directoryRenames = false
[diff]
	renames = true
[core]
	autocrlf = false
	filemode = true
[user]
	name = Merge Fixture
	email = fixture@example.invalid
[init]
	defaultBranch = main
`

func gitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Merge Fixture",
		"GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Merge Fixture",
		"GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_DATE=1700000000 +0000",
	}
}

func gitBin() string {
	if g := os.Getenv("GIT"); g != "" {
		return g
	}
	return "git"
}

func run(env []string, dir string, stdin []byte, args ...string) error {
	cmd := exec.Command(gitBin(), args...)
	cmd.Dir = dir
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

func stream(sc *scenario) []byte {
	var b bytes.Buffer
	const t0 = 1700000000
	for i, c := range sc.commits {
		fmt.Fprintf(&b, "commit refs/gen/tmp\nmark :%d\n", c.mark)
		ts := t0 + 3600*(i+1)
		fmt.Fprintf(&b, "author Merge Fixture <fixture@example.invalid> %d +0000\n", ts)
		fmt.Fprintf(&b, "committer Merge Fixture <fixture@example.invalid> %d +0000\n", ts)
		msg := c.msg + "\n"
		fmt.Fprintf(&b, "data %d\n%s", len(msg), msg)
		for j, p := range c.parents {
			if j == 0 {
				fmt.Fprintf(&b, "from :%d\n", p)
			} else {
				fmt.Fprintf(&b, "merge :%d\n", p)
			}
		}
		b.WriteString("deleteall\n")
		for _, p := range c.t.paths() {
			e := c.t[p]
			fmt.Fprintf(&b, "M %s inline %s\ndata %d\n%s\n", e.mode, p, len(e.data), e.data)
		}
		b.WriteString("\n")
	}
	names := make([]string, 0, len(sc.refs))
	for n := range sc.refs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "reset refs/heads/%s\nfrom :%d\n\n", n, sc.refs[n])
	}
	b.WriteString("done\n")
	return b.Bytes()
}

func write(sc *scenario, out, home string) error {
	path := filepath.Join(out, sc.name)
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	env := gitEnv(home)
	args := []string{"init", "-q", "--template=", "-b", "main"}
	if sc.bare {
		args = append(args, "--bare")
	}
	if err := run(env, out, nil, append(args, path)...); err != nil {
		return err
	}
	gitDir := path
	if !sc.bare {
		gitDir = filepath.Join(path, ".git")
	}
	cfg, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := cfg.WriteString(repoConfig); err != nil {
		cfg.Close()
		return err
	}
	cfg.Close()
	// Keep the objects in one pack (fast-import would explode a small import
	// into loose objects), which keeps a corpus of a thousand repositories
	// small and quick to delete.
	if err := run(env, path, stream(sc), "-c", "fastimport.unpackLimit=0", "fast-import", "--quiet", "--done"); err != nil {
		return err
	}
	if err := run(env, path, nil, "update-ref", "-d", "refs/gen/tmp"); err != nil {
		return err
	}
	if !sc.bare {
		if err := run(env, path, nil, "checkout", "-q", "-f", sc.head); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	out := flag.String("out", "", "directory to create the repositories in")
	seed := flag.Uint64("seed", 0, "first seed")
	count := flag.Int("count", 1, "number of consecutive seeds")
	caseName := flag.String("case", "", "hand-built case name, or \"all\"")
	list := flag.Bool("list", false, "list hand-built cases and exit")
	jobs := flag.Int("j", 4, "parallel jobs")
	flag.Parse()

	if *list {
		for _, c := range handCases() {
			fmt.Printf("%-22s %s\n", c.name, c.desc)
		}
		return
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "gen: -out is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	abs, _ := filepath.Abs(*out)
	home := filepath.Join(abs, ".home")
	_ = os.MkdirAll(home, 0o755)

	var scs []*scenario
	if *caseName != "" {
		for _, c := range handCases() {
			if *caseName == "all" || *caseName == c.name {
				scs = append(scs, c.make())
			}
		}
		if len(scs) == 0 {
			fmt.Fprintf(os.Stderr, "gen: unknown case %q\n", *caseName)
			os.Exit(2)
		}
	} else {
		if *seed == 0 {
			fmt.Fprintln(os.Stderr, "gen: -seed must be positive")
			os.Exit(2)
		}
		for i := 0; i < *count; i++ {
			scs = append(scs, seeded(*seed+uint64(i)))
		}
	}

	errs := make([]error, len(scs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, *jobs))
	for i, sc := range scs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, sc *scenario) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = write(sc, abs, home)
		}(i, sc)
	}
	wg.Wait()
	failed := false
	for i, sc := range scs {
		if errs[i] != nil {
			fmt.Fprintf(os.Stderr, "gen: %s: %v\n", sc.name, errs[i])
			failed = true
			continue
		}
		fmt.Printf("%s %s %s\n", sc.name, sc.family, filepath.Join(abs, sc.name))
	}
	if failed {
		os.Exit(1)
	}
}
