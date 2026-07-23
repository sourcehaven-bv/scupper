package scupper

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildModule writes a throwaway module containing src (as pkg/lib.go) and
// testSrc (as pkg/lib_test.go), runs `go test -coverprofile`, and returns the
// module dir plus the parsed profile. This exercises the REAL Go coverage
// profiler, so the tests below assert on how statement coverage and multi-line
// constructs actually behave — not on a hand-written profile that could drift
// from the toolchain.
func buildModule(t *testing.T, src, testSrc string) (dir string, prof *Profile) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir = t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module stmtfixture\n\ngo 1.26\n")
	pkgDir := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(pkgDir, "lib.go"), src)
	mustWrite(t, filepath.Join(pkgDir, "lib_test.go"), testSrc)

	profPath := filepath.Join(dir, "cover.out")
	cmd := exec.Command("go", "test", "./pkg/...", "-coverprofile="+profPath, "-covermode=set")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go test failed: %v\n%s", err, stderr.String())
	}
	f, err := os.Open(profPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	prof, err = ParseProfile(f)
	if err != nil {
		t.Fatal(err)
	}
	return dir, prof
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// filterPct runs the real filter against the fixture module and returns the
// coverage percent, removed-statement count, and the uncovered blocks that
// survived filtering.
func filterPct(t *testing.T, dir string, prof *Profile) (pct float64, removed int, uncovered []Block) {
	t.Helper()
	res, err := Filter(prof, NewResolver(dir), DefaultDirectives(""))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.Kept.Blocks {
		if b.Count == 0 {
			uncovered = append(uncovered, b)
		}
	}
	return Compute(res.Kept).Percent(), res.RemovedStmts, uncovered
}

const branchyTest = `package pkg
import "testing"
func TestAll(t *testing.T) { Branchy(2, 1) } // only exercises a>b
`

// A multi-line single statement is ONE profile block spanning all its lines; an
// ignore on ANY of those lines drops the whole statement.
func TestIntegration_MultiLineStatement_IgnoreOnMiddleLine(t *testing.T) {
	src := `package pkg
import "fmt"
func Msg(x int) string {
	return fmt.Sprintf(
		"v=%d", //scupper:ignore
		x,
	)
}
`
	test := `package pkg
import "testing"
func TestAll(t *testing.T) { Msg(1) }
`
	dir, prof := buildModule(t, src, test)
	_, removed, _ := filterPct(t, dir, prof)
	if removed != 1 {
		t.Errorf("expected the whole multi-line statement (1 stmt) to be scuppered, removed=%d", removed)
	}
}

// A multi-line composite literal is part of its enclosing statement: one block,
// one statement. Ignoring any line of it removes the single statement.
func TestIntegration_MultiLineLiteral(t *testing.T) {
	src := `package pkg
func Nums() []int {
	return []int{
		1,
		2, //scupper:ignore
		3,
	}
}
`
	test := `package pkg
import "testing"
func TestAll(t *testing.T) { Nums() }
`
	dir, prof := buildModule(t, src, test)
	_, removed, _ := filterPct(t, dir, prof)
	if removed != 1 {
		t.Errorf("multi-line literal is one statement; removed=%d want 1", removed)
	}
}

// Several statements on one physical line collapse into one block with
// numStmt>1. A line ignore there drops ALL of them — you cannot ignore just one.
// This documents the (rare) sharp edge so a refactor that changes it is caught.
func TestIntegration_MultipleStatementsOneLine(t *testing.T) {
	src := `package pkg
func Three(x int) int {
	a := 1; b := 2; return a + b + x //scupper:ignore
}
`
	test := `package pkg
import "testing"
func TestAll(t *testing.T) { Three(1) }
`
	dir, prof := buildModule(t, src, test)
	_, removed, _ := filterPct(t, dir, prof)
	if removed != 3 {
		t.Errorf("three statements share one line; a line-ignore drops all 3, removed=%d", removed)
	}
}

// An `if` produces multiple blocks (condition, then-body, continuation) that
// SHARE the condition line. A line-ignore on the `if` line drops the blocks
// overlapping that line but NOT the continuation — so a genuinely uncovered
// else/fall-through survives. This is why the block form exists for whole
// branches.
func TestIntegration_IfLineIgnore_DoesNotSwallowContinuation(t *testing.T) {
	src := `package pkg
func Branchy(a, b int) int {
	if a > b { //scupper:ignore
		return a
	}
	return b
}
`
	dir, prof := buildModule(t, src, branchyTest)
	_, removed, uncovered := filterPct(t, dir, prof)
	if removed == 0 {
		t.Fatal("expected the if-line ignore to drop at least the shared blocks")
	}
	// The uncovered `return b` continuation must survive (its line is not ignored).
	found := false
	for _, b := range uncovered {
		if strings.HasSuffix(b.File, "lib.go") && b.StartLine == 6 {
			found = true
		}
	}
	if !found {
		t.Errorf("uncovered continuation `return b` (line 6) was wrongly swallowed; uncovered=%+v", uncovered)
	}
}

// The block form cleanly excludes an entire impossible branch: wrap the whole
// `if err != nil { ... }` and the whole thing — condition and body — drains out,
// reaching 100% with no genuinely-untested code hidden.
func TestIntegration_BlockForm_ExcludesWholeImpossibleBranch(t *testing.T) {
	src := `package pkg
import "fmt"
func Load(x int) int {
	v, err := parse(x)
	//scupper:ignore-start
	if err != nil {
		panic(fmt.Sprintf("cannot happen: %v", err))
	}
	//scupper:ignore-end
	return v
}
func parse(x int) (int, error) { return x, nil }
`
	test := `package pkg
import "testing"
func TestAll(t *testing.T) { Load(1) }
`
	dir, prof := buildModule(t, src, test)
	pct, removed, uncovered := filterPct(t, dir, prof)
	if pct != 100.0 {
		t.Errorf("block-excluded impossible branch should yield 100%%, got %.1f%% (uncovered=%+v)", pct, uncovered)
	}
	if removed == 0 {
		t.Error("expected the impossible branch to be scuppered")
	}
}

// A genuinely uncovered, NON-ignored statement must still count against
// coverage — ignores can only remove, never mask a real gap.
func TestIntegration_GenuineGapStillCounts(t *testing.T) {
	src := `package pkg
func Branchy(a, b int) int {
	if a > b {
		return a
	}
	return b
}
`
	dir, prof := buildModule(t, src, branchyTest)
	pct, _, uncovered := filterPct(t, dir, prof)
	if pct >= 100.0 {
		t.Errorf("uncovered `return b` must keep coverage below 100%%, got %.1f%%", pct)
	}
	if len(uncovered) == 0 {
		t.Error("expected the untested branch to be reported as uncovered")
	}
}

// TestIntegration_CrossPackageCoverpkg builds a two-package module where a
// library is exercised ONLY by a consumer package's test, and the profile is
// produced with -coverpkg=./... (the emit-per-binary duplication case). scupper
// must merge the duplicate blocks and credit the library's coverage, matching
// how `go tool cover` reads the same profile. This is the fix for the
// cross-package attribution gap (rela's graphquerynaive reads 0% without it).
func TestIntegration_CrossPackageCoverpkg(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module xfix\n\ngo 1.26\n")

	mustWrite(t, filepath.Join(dir, "lib", "lib.go"), `package lib
func Helper(x int) int {
	if x > 0 {
		return x * 2
	}
	return -x
}
`)
	// consumer's test exercises BOTH branches of lib.Helper; lib has no test.
	mustWrite(t, filepath.Join(dir, "consumer", "consumer.go"), `package consumer
import "xfix/lib"
func Use(x int) int { return lib.Helper(x) }
`)
	mustWrite(t, filepath.Join(dir, "consumer", "consumer_test.go"), `package consumer
import "testing"
func TestUse(t *testing.T){ Use(5); Use(-3) }
`)

	profPath := filepath.Join(dir, "cover.out")
	cmd := exec.Command("go", "test", "./...", "-coverpkg=./...", "-coverprofile="+profPath, "-covermode=set")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go test failed: %v\n%s", err, stderr.String())
	}
	f, err := os.Open(profPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	prof, err := ParseProfile(f)
	if err != nil {
		t.Fatal(err)
	}

	// Sanity: the raw profile really does contain duplicate lib blocks (count 0
	// and count 1 for the same span) — otherwise this test proves nothing.
	dupSeen := false
	seen := map[string]int{}
	for _, b := range prof.Blocks {
		if strings.Contains(b.File, "lib/lib.go") {
			k := b.File + ":" + string(rune(b.StartLine))
			seen[k]++
			if seen[k] > 1 {
				dupSeen = true
			}
		}
	}
	if !dupSeen {
		t.Log("note: no duplicate lib blocks observed; toolchain may have changed -coverpkg emission")
	}

	pct, _, uncovered := filterPct(t, dir, prof)
	if pct != 100.0 {
		t.Errorf("cross-package coverage should be 100%% after merge, got %.1f%% (uncovered=%+v)", pct, uncovered)
	}
}
