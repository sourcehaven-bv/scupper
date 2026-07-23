package scupper

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Block is one entry in a Go coverage profile: a half-open span of source with
// a statement count and a hit count.
//
//	file.go:startLine.startCol,endLine.endCol numStmt count
type Block struct {
	File      string // profile file token (import-path based), verbatim
	StartLine int
	StartCol  int
	EndLine   int
	EndCol    int
	NumStmt   int
	Count     int
}

// Profile is a parsed coverage profile.
type Profile struct {
	Mode   string
	Blocks []Block
}

// ParseProfile reads a Go coverage profile (the output of `go test
// -coverprofile`).
func ParseProfile(r io.Reader) (*Profile, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	p := &Profile{}
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			if !strings.HasPrefix(line, "mode:") {
				return nil, fmt.Errorf("invalid profile: first line must be 'mode:', got %q", line)
			}
			p.Mode = strings.TrimSpace(strings.TrimPrefix(line, "mode:"))
			first = false
			continue
		}
		b, err := parseBlock(line)
		if err != nil {
			return nil, err
		}
		p.Blocks = append(p.Blocks, b)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if first {
		return nil, fmt.Errorf("invalid profile: empty or missing mode line")
	}
	return p, nil
}

// blockKey identifies a block by its span and statement count — everything
// except the hit count. Two blocks with the same key are "the same block"
// counted in different test runs.
type blockKey struct {
	File                     string
	StartLine, StartCol      int
	EndLine, EndCol, NumStmt int
}

func (b Block) key() blockKey {
	return blockKey{b.File, b.StartLine, b.StartCol, b.EndLine, b.EndCol, b.NumStmt}
}

// Merge collapses duplicate blocks — blocks sharing a span and statement count
// that appear more than once — into a single block whose count is the max of
// the duplicates. This is required to read a profile produced with
// `go test -coverpkg=./...`, where a package's blocks are emitted once per test
// binary that instruments it (count 0 from a binary that never runs them, count
// >0 from one that does). Taking the max means "covered by ANY run", matching
// how `go tool cover` and go-test-coverage merge such profiles. Without this,
// the same statement is counted several times — once covered, once not —
// producing a coverage number that is both wrong and below reality.
//
// Block order is preserved by first appearance. A profile without duplicates is
// returned unchanged in effect (every block maps to itself).
func (p *Profile) Merge() *Profile {
	idx := make(map[blockKey]int, len(p.Blocks))
	out := &Profile{Mode: p.Mode, Blocks: make([]Block, 0, len(p.Blocks))}
	for _, b := range p.Blocks {
		k := b.key()
		if i, seen := idx[k]; seen {
			if b.Count > out.Blocks[i].Count {
				out.Blocks[i].Count = b.Count
			}
			continue
		}
		idx[k] = len(out.Blocks)
		out.Blocks = append(out.Blocks, b)
	}
	return out
}

// parseBlock parses one profile data line:
// "path/file.go:3.24,4.11 1 0". The filename may itself contain colons on
// exotic systems, so split on the LAST colon before the position spec.
func parseBlock(line string) (Block, error) {
	sp := strings.Fields(line)
	if len(sp) != 3 {
		return Block{}, fmt.Errorf("malformed profile line %q", line)
	}
	numStmt, err := strconv.Atoi(sp[1])
	if err != nil {
		return Block{}, fmt.Errorf("bad statement count in %q: %w", line, err)
	}
	count, err := strconv.Atoi(sp[2])
	if err != nil {
		return Block{}, fmt.Errorf("bad hit count in %q: %w", line, err)
	}
	// sp[0] is file:startLine.startCol,endLine.endCol
	colon := strings.LastIndex(sp[0], ":")
	if colon < 0 {
		return Block{}, fmt.Errorf("malformed span in %q", line)
	}
	file := sp[0][:colon]
	startSpan, endSpan, ok := strings.Cut(sp[0][colon+1:], ",")
	if !ok {
		return Block{}, fmt.Errorf("malformed span in %q", line)
	}
	sl, sc, err := parseLineCol(startSpan)
	if err != nil {
		return Block{}, fmt.Errorf("in %q: %w", line, err)
	}
	el, ec, err := parseLineCol(endSpan)
	if err != nil {
		return Block{}, fmt.Errorf("in %q: %w", line, err)
	}
	return Block{file, sl, sc, el, ec, numStmt, count}, nil
}

func parseLineCol(s string) (line, col int, err error) {
	ls, cs, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, fmt.Errorf("malformed line.col %q", s)
	}
	line, err = strconv.Atoi(ls)
	if err != nil {
		return 0, 0, err
	}
	col, err = strconv.Atoi(cs)
	if err != nil {
		return 0, 0, err
	}
	return line, col, nil
}

// Resolver maps a profile file token (import-path based, e.g.
// "example.com/mod/pkg/file.go") to an absolute path on disk. Resolution uses
// `go list` for the packages it encounters, cached per package directory.
type Resolver struct {
	dir   string            // directory to run `go list` in
	cache map[string]string // import path -> package dir
}

// NewResolver returns a Resolver that resolves packages relative to dir (the
// module root or any dir inside the module).
func NewResolver(dir string) *Resolver {
	return &Resolver{dir: dir, cache: map[string]string{}}
}

// Resolve returns the absolute filesystem path for a profile file token. The
// token is "<import path of package>/<basename>.go". If the file already exists
// as given (relative or absolute), that path is returned directly — this covers
// profiles written with real paths.
func (r *Resolver) Resolve(token string) (string, error) {
	if filepath.IsAbs(token) {
		return token, nil
	}
	if _, err := os.Stat(token); err == nil {
		abs, _ := filepath.Abs(token)
		return abs, nil
	}
	importPath := filepath.ToSlash(filepath.Dir(token))
	base := filepath.Base(token)
	pkgDir, ok := r.cache[importPath]
	if !ok {
		out, err := r.goList(importPath)
		if err != nil {
			return "", err
		}
		pkgDir = out
		r.cache[importPath] = pkgDir
	}
	return filepath.Join(pkgDir, base), nil
}

func (r *Resolver) goList(importPath string) (string, error) {
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", importPath)
	cmd.Dir = r.dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go list %s: %w", importPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// FilterResult reports what Filter did.
type FilterResult struct {
	Kept         *Profile
	RemovedStmts int // statements dropped because they were in ignored ranges
	IgnoredFiles []string
}

// Filter removes profile blocks that fall within ignored ranges of their source
// file. A block is removed if its start line is within an ignored range; this
// matches the intent of marking a statement or branch as ignored. Whole-file
// ignores drop every block for that file.
//
// scanCache memoizes FileIgnore per resolved path so each source file is read
// once.
func Filter(p *Profile, res *Resolver, d Directives) (*FilterResult, error) {
	// Collapse duplicate blocks first so a -coverpkg profile (where each block
	// appears once per instrumenting test binary) is counted correctly.
	p = p.Merge()

	scanCache := map[string]FileIgnore{}
	ignoredFiles := map[string]bool{}
	out := &Profile{Mode: p.Mode}
	var removed int

	for _, b := range p.Blocks {
		path, err := res.Resolve(b.File)
		if err != nil {
			return nil, err
		}
		fi, ok := scanCache[path]
		if !ok {
			fi, err = ScanFile(path, d)
			if err != nil {
				return nil, err
			}
			scanCache[path] = fi
		}
		if fi.WholeFile {
			ignoredFiles[b.File] = true
			removed += b.NumStmt
			continue
		}
		// A block is ignored if any of its source lines is ignored. A directive
		// may sit on any line of the statement it guards (e.g. the `return` line
		// of an `if`), and a Go profile block can span several lines, so we test
		// the whole [StartLine,EndLine] span rather than just the start.
		if fi.OverlapsRange(b.StartLine, b.EndLine) {
			removed += b.NumStmt
			continue
		}
		out.Blocks = append(out.Blocks, b)
	}

	files := make([]string, 0, len(ignoredFiles))
	for f := range ignoredFiles {
		files = append(files, f)
	}
	sort.Strings(files)
	return &FilterResult{Kept: out, RemovedStmts: removed, IgnoredFiles: files}, nil
}

// Write serializes a profile back to the standard textual format.
func (p *Profile) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "mode: %s\n", p.Mode)
	for _, b := range p.Blocks {
		fmt.Fprintf(bw, "%s:%d.%d,%d.%d %d %d\n",
			b.File, b.StartLine, b.StartCol, b.EndLine, b.EndCol, b.NumStmt, b.Count)
	}
	return bw.Flush()
}

// Stats summarizes coverage over a profile.
type Stats struct {
	TotalStmts   int
	CoveredStmts int
}

// Percent returns line/statement coverage as a percentage. An empty profile
// (no statements) is defined as 100% covered — there is nothing left uncovered.
func (s Stats) Percent() float64 {
	if s.TotalStmts == 0 {
		return 100.0
	}
	return 100.0 * float64(s.CoveredStmts) / float64(s.TotalStmts)
}

// Compute returns coverage statistics for a profile.
func Compute(p *Profile) Stats {
	var s Stats
	for _, b := range p.Blocks {
		s.TotalStmts += b.NumStmt
		if b.Count > 0 {
			s.CoveredStmts += b.NumStmt
		}
	}
	return s
}
