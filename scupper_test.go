package scupper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "src.go")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanFile_SingleLine(t *testing.T) {
	p := writeTemp(t, `package x
func f() {
	a := 1 //scupper:ignore
	_ = a
}
`)
	fi, err := ScanFile(p, DefaultDirectives(""))
	if err != nil {
		t.Fatal(err)
	}
	if fi.WholeFile {
		t.Fatal("did not expect whole-file ignore")
	}
	if !fi.Covers(3) {
		t.Error("line 3 should be ignored")
	}
	if fi.Covers(4) {
		t.Error("line 4 should not be ignored")
	}
}

func TestScanFile_Block(t *testing.T) {
	p := writeTemp(t, `package x
func f() {
	//scupper:ignore-start
	a := 1
	b := 2
	//scupper:ignore-end
	_ = a
}
`)
	fi, err := ScanFile(p, DefaultDirectives(""))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{3, 4, 5, 6} {
		if !fi.Covers(n) {
			t.Errorf("line %d should be ignored", n)
		}
	}
	if fi.Covers(7) {
		t.Error("line 7 should not be ignored")
	}
}

func TestScanFile_WholeFile(t *testing.T) {
	p := writeTemp(t, `//scupper:ignore-file
package x
func f() {}
`)
	fi, err := ScanFile(p, DefaultDirectives(""))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.WholeFile {
		t.Fatal("expected whole-file ignore")
	}
	if !fi.Covers(999) {
		t.Error("whole-file ignore should cover any line")
	}
}

func TestScanFile_UnterminatedBlockErrors(t *testing.T) {
	p := writeTemp(t, `package x
//scupper:ignore-start
func f() {}
`)
	_, err := ScanFile(p, DefaultDirectives(""))
	if err == nil {
		t.Fatal("expected error for unterminated ignore block")
	}
	if !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("error should mention 'unterminated', got %v", err)
	}
}

func TestScanFile_SwappedEndBeforeStart(t *testing.T) {
	// A stray/swapped end (end appearing before its start) must be a hard error,
	// not a silent no-op.
	p := writeTemp(t, `package x
//scupper:ignore-end
code
//scupper:ignore-start
more
`)
	_, err := ScanFile(p, DefaultDirectives(""))
	if err == nil {
		t.Fatal("expected error for end-before-start")
	}
	if !strings.Contains(err.Error(), "no open block") {
		t.Errorf("error should explain the stray end, got %v", err)
	}
}

func TestScanFile_StrayEnd(t *testing.T) {
	// An ignore-end with no matching start anywhere must error.
	p := writeTemp(t, `package x
//scupper:ignore-end
`)
	_, err := ScanFile(p, DefaultDirectives(""))
	if err == nil || !strings.Contains(err.Error(), "no open block") {
		t.Errorf("stray end should error with 'no open block', got %v", err)
	}
}

func TestScanFile_NestedStartErrors(t *testing.T) {
	// Blocks do not nest: a second start while one is open must error rather
	// than silently flatten the user's intent.
	p := writeTemp(t, `package x
//scupper:ignore-start
a
//scupper:ignore-start
b
//scupper:ignore-end
c
//scupper:ignore-end
`)
	_, err := ScanFile(p, DefaultDirectives(""))
	if err == nil || !strings.Contains(err.Error(), "already open") {
		t.Errorf("nested start should error with 'already open', got %v", err)
	}
}

func TestScanFile_CustomDirective(t *testing.T) {
	p := writeTemp(t, `package x
func f() {
	a := 1 //nocov
	_ = a
}
`)
	fi, err := ScanFile(p, DefaultDirectives("nocov"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Covers(3) {
		t.Error("line 3 should be ignored with custom directive")
	}
}

func TestScanFile_LongestMatchWins(t *testing.T) {
	// "scupper:ignore" is a prefix of "scupper:ignore-file"; ensure the
	// whole-file directive is not misread as a plain line ignore.
	p := writeTemp(t, `//scupper:ignore-file
package x
`)
	fi, err := ScanFile(p, DefaultDirectives(""))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.WholeFile {
		t.Error("ignore-file must take precedence over the bare line directive")
	}
}

func TestScanFile_ProseMentionDoesNotTrigger(t *testing.T) {
	// A directive keyword mentioned inside prose (documentation) must NOT be
	// treated as a directive; only a comment whose first token is the keyword.
	p := writeTemp(t, `package x
// This function uses scupper:ignore semantics but is not a directive.
// See the //scupper:ignore docs for details.
func f() int { return 1 }
`)
	fi, err := ScanFile(p, DefaultDirectives(""))
	if err != nil {
		t.Fatal(err)
	}
	if fi.WholeFile || len(fi.Ranges) != 0 {
		t.Errorf("prose mention wrongly triggered a directive: %+v", fi)
	}
}

func TestScanFile_TrailingNoteAfterDirective(t *testing.T) {
	p := writeTemp(t, `package x
func f() int {
	return 2 //scupper:ignore not yet worth testing
}
`)
	fi, err := ScanFile(p, DefaultDirectives(""))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Covers(3) {
		t.Error("directive with a trailing human note should still fire")
	}
}

func TestScanFile_CoverageIgnoreBaseWithColonAndReason(t *testing.T) {
	// The go-test-coverage style: `// coverage-ignore: <reason>` with a colon.
	p := writeTemp(t, `package x
func f() int {
	return 2 // coverage-ignore: defensive, cannot fail here
}
`)
	d := DefaultDirectives("coverage-ignore")
	fi, err := ScanFile(p, d)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Covers(3) {
		t.Error("coverage-ignore: with a colon+reason should fire on line 3")
	}
}

func TestScanFile_CoverageIgnoreBlockAndFile(t *testing.T) {
	p := writeTemp(t, `// coverage-ignore-file: generated code
package x
func f() {
	// coverage-ignore-start: impossible branch
	panic("x")
	// coverage-ignore-end
}
`)
	fi, err := ScanFile(p, DefaultDirectives("coverage-ignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.WholeFile {
		t.Error("coverage-ignore-file should set WholeFile")
	}
}

func TestScanFile_IgnoreFunc(t *testing.T) {
	// A directive on the line above a func dismisses the WHOLE function span
	// (the go-test-coverage "whole function" convention, made explicit).
	p := writeTemp(t, `package x

// coverage-ignore-func: startup wiring
func Untested(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func Tested() int { return 1 }
`)
	fi, err := ScanFile(p, DefaultDirectives("coverage-ignore"))
	if err != nil {
		t.Fatal(err)
	}
	// Untested spans lines 4-9; every line of it must be ignored.
	for _, n := range []int{4, 5, 6, 7, 8, 9} {
		if !fi.Covers(n) {
			t.Errorf("line %d (inside Untested) should be ignored", n)
		}
	}
	// Tested (line 12) must NOT be ignored.
	if fi.Covers(12) {
		t.Error("Tested must not be ignored")
	}
}

func TestScanFile_IgnoreFunc_Method(t *testing.T) {
	// Works for methods (FuncDecl with a receiver) too.
	p := writeTemp(t, `package x

type T struct{}

// coverage-ignore-func: unreachable
func (T) M(x int) int {
	if x > 0 {
		return x
	}
	return 0
}
`)
	fi, err := ScanFile(p, DefaultDirectives("coverage-ignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{6, 7, 8, 9, 10, 11} {
		if !fi.Covers(n) {
			t.Errorf("line %d (inside method M) should be ignored", n)
		}
	}
}

func TestScanFile_IgnoreFunc_NoFuncBelowErrors(t *testing.T) {
	// A func directive with no function under it is a hard error, not a silent
	// swallow.
	p := writeTemp(t, `package x

func Above() int { return 1 }

// coverage-ignore-func: dangling
var x = 1
`)
	_, err := ScanFile(p, DefaultDirectives("coverage-ignore"))
	if err == nil || !strings.Contains(err.Error(), "no function declaration below") {
		t.Errorf("dangling ignore-func should error, got %v", err)
	}
}

func TestScanFile_IgnoreFunc_ScupperDefaultBase(t *testing.T) {
	// The default base yields //scupper:ignore-func.
	p := writeTemp(t, `package x

//scupper:ignore-func impossible
func F() int {
	return 1
}
`)
	fi, err := ScanFile(p, DefaultDirectives(""))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Covers(4) || !fi.Covers(5) {
		t.Errorf("scupper:ignore-func should ignore the function body, ranges=%+v", fi.Ranges)
	}
}

func TestScanFile_RequireReason(t *testing.T) {
	d := DefaultDirectives("coverage-ignore")
	d.RequireReason = true

	// Missing reason -> error.
	bare := writeTemp(t, `package x
func f() int {
	return 2 // coverage-ignore
}
`)
	if _, err := ScanFile(bare, d); err == nil || !strings.Contains(err.Error(), "requires an explanation") {
		t.Errorf("bare directive under RequireReason should error, got %v", err)
	}

	// With reason -> ok.
	withReason := writeTemp(t, `package x
func f() int {
	return 2 // coverage-ignore: because
}
`)
	if _, err := ScanFile(withReason, d); err != nil {
		t.Errorf("directive with reason should pass, got %v", err)
	}
}

func TestScanFile_RequireReason_EndExempt(t *testing.T) {
	// -end carries no reason (the -start does); RequireReason must not flag it.
	d := DefaultDirectives("coverage-ignore")
	d.RequireReason = true
	p := writeTemp(t, `package x
func f() {
	// coverage-ignore-start: impossible
	panic("x")
	// coverage-ignore-end
}
`)
	if _, err := ScanFile(p, d); err != nil {
		t.Errorf("coverage-ignore-end must be exempt from RequireReason, got %v", err)
	}
}

func TestOverlapsRange(t *testing.T) {
	fi := FileIgnore{Ranges: []Range{{10, 12}}}
	cases := []struct {
		lo, hi int
		want   bool
	}{
		{10, 10, true},  // start
		{12, 12, true},  // end
		{9, 10, true},   // straddles start
		{12, 15, true},  // straddles end
		{8, 20, true},   // encloses
		{1, 9, false},   // before
		{13, 20, false}, // after
	}
	for _, c := range cases {
		if got := fi.OverlapsRange(c.lo, c.hi); got != c.want {
			t.Errorf("OverlapsRange(%d,%d) = %v, want %v", c.lo, c.hi, got, c.want)
		}
	}
}

func TestParseProfile_RoundTrip(t *testing.T) {
	in := "mode: set\nexample.com/m/f.go:3.24,4.11 1 1\nexample.com/m/f.go:4.11,6.3 2 0\n"
	p, err := ParseProfile(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != "set" {
		t.Errorf("mode = %q, want set", p.Mode)
	}
	if len(p.Blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(p.Blocks))
	}
	b := p.Blocks[1]
	if b.File != "example.com/m/f.go" || b.StartLine != 4 || b.EndCol != 3 || b.NumStmt != 2 || b.Count != 0 {
		t.Errorf("unexpected block: %+v", b)
	}
	var sb strings.Builder
	if err := p.Write(&sb); err != nil {
		t.Fatal(err)
	}
	if sb.String() != in {
		t.Errorf("round-trip mismatch:\n got %q\nwant %q", sb.String(), in)
	}
}

func TestParseProfile_Errors(t *testing.T) {
	for _, in := range []string{
		"",
		"example.com/f.go:1.1,2.2 1 0\n", // missing mode line
		"mode: set\ngarbage line here\n",
	} {
		if _, err := ParseProfile(strings.NewReader(in)); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestMerge_CollapsesDuplicateBlocksByMaxCount(t *testing.T) {
	// A -coverpkg profile emits the same block once per instrumenting binary:
	// count 0 from a run that never executes it, count 1 from one that does.
	// Merge must collapse them to a single covered block.
	in := "mode: set\n" +
		"m/f.go:1.1,2.2 1 0\n" +
		"m/f.go:1.1,2.2 1 1\n" + // same span, covered elsewhere
		"m/f.go:3.1,4.2 1 0\n" // genuinely uncovered, appears once
	p, err := ParseProfile(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	m := p.Merge()
	if len(m.Blocks) != 2 {
		t.Fatalf("expected 2 merged blocks, got %d: %+v", len(m.Blocks), m.Blocks)
	}
	if m.Blocks[0].Count != 1 {
		t.Errorf("duplicate block should merge to covered (max count 1), got %d", m.Blocks[0].Count)
	}
	if m.Blocks[1].Count != 0 {
		t.Errorf("the single uncovered block must stay uncovered, got %d", m.Blocks[1].Count)
	}
	s := Compute(m)
	if s.TotalStmts != 2 || s.CoveredStmts != 1 {
		t.Errorf("merged stats = %+v, want total 2 covered 1", s)
	}
}

func TestMerge_NoDuplicatesIsIdentity(t *testing.T) {
	in := "mode: set\nm/f.go:1.1,2.2 1 1\nm/f.go:3.1,4.2 2 0\n"
	p, _ := ParseProfile(strings.NewReader(in))
	m := p.Merge()
	if len(m.Blocks) != len(p.Blocks) {
		t.Fatalf("merge changed block count on a duplicate-free profile: %d -> %d", len(p.Blocks), len(m.Blocks))
	}
	for i := range p.Blocks {
		if p.Blocks[i] != m.Blocks[i] {
			t.Errorf("block %d changed: %+v -> %+v", i, p.Blocks[i], m.Blocks[i])
		}
	}
}

func TestCompute(t *testing.T) {
	p := &Profile{Blocks: []Block{
		{NumStmt: 3, Count: 1},
		{NumStmt: 2, Count: 0},
		{NumStmt: 1, Count: 5},
	}}
	s := Compute(p)
	if s.TotalStmts != 6 || s.CoveredStmts != 4 {
		t.Errorf("stats = %+v, want total 6 covered 4", s)
	}
	if got := s.Percent(); got < 66.6 || got > 66.7 {
		t.Errorf("percent = %.2f, want ~66.67", got)
	}
	if empty := (Stats{}); empty.Percent() != 100.0 {
		t.Error("empty profile should be 100%")
	}
}

func TestFilter(t *testing.T) {
	// Build a package on disk so the resolver can find it via `go list`.
	dir := t.TempDir()
	mod := "scuppertest"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+mod+"\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `package p
func Keep() int { return 1 }
func Drop() int {
	//scupper:ignore-start
	return 2
	//scupper:ignore-end
}
`
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// Line 2 = Keep body (uncovered here), line 5 = Drop body (inside ignore block).
	prof := &Profile{Mode: "set", Blocks: []Block{
		{File: mod + "/p.go", StartLine: 2, StartCol: 18, EndLine: 2, EndCol: 28, NumStmt: 1, Count: 0},
		{File: mod + "/p.go", StartLine: 5, StartCol: 2, EndLine: 5, EndCol: 10, NumStmt: 1, Count: 0},
	}}
	res := NewResolver(dir)
	r, err := Filter(prof, res, DefaultDirectives(""))
	if err != nil {
		t.Fatal(err)
	}
	if r.RemovedStmts != 1 {
		t.Errorf("removed %d stmts, want 1", r.RemovedStmts)
	}
	if len(r.Kept.Blocks) != 1 || r.Kept.Blocks[0].StartLine != 2 {
		t.Errorf("kept blocks = %+v, want only the Keep block", r.Kept.Blocks)
	}
}
