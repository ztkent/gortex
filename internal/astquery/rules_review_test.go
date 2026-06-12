package astquery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReviewRulesCompile drives every registered review detector
// against a stub source per language so a tree-sitter pattern that
// compiles in isolation but not under the engine fails at unit-test
// time.
func TestReviewRulesCompile(t *testing.T) {
	stubs := map[string]struct {
		file string
		src  string
	}{
		"go":     {"sample.go", "package x\n"},
		"python": {"sample.py", "x = 1\n"},
	}
	for _, info := range DescribeDetectors() {
		if info.Category != CategoryReview {
			continue
		}
		for _, lang := range info.Languages {
			stub, ok := stubs[lang]
			if !ok {
				continue
			}
			lang := lang
			t.Run(info.Name+"/"+lang, func(t *testing.T) {
				_, err := RunOnSource(context.Background(), Options{Detector: info.Name},
					stub.file, lang, []byte(stub.src))
				require.NoError(t, err, "review rule %s failed to compile for %s", info.Name, lang)
			})
		}
	}
}

// TestReviewRulesFire pairs each review rule with a positive fixture
// (must fire) and a negative fixture (must be silent), for Go and
// Python.
func TestReviewRulesFire(t *testing.T) {
	cases := []struct {
		name string
		lang string
		file string
		bad  string
		good string
	}{
		// --- Go ---------------------------------------------------------
		{"go-unchecked-type-assertion", "go", "lib.go",
			"package x\nfunc f(y any) string {\n\tv := y.(string)\n\treturn v\n}\n",
			"package x\nfunc f(y any) (string, bool) {\n\tv, ok := y.(string)\n\treturn v, ok\n}\n"},
		{"go-inverted-err-check", "go", "lib.go",
			"package x\nfunc f() error {\n\terr := do()\n\tif err == nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n",
			"package x\nfunc f() error {\n\terr := do()\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n"},
		{"go-check-then-act-map", "go", "lib.go",
			"package x\nfunc f(m map[string]int, k string) {\n\tif _, ok := m[k]; !ok {\n\t\tm[k] = 1\n\t}\n}\n",
			"package x\nfunc f(m map[string]int, k string) {\n\tif _, ok := m[k]; !ok {\n\t\tcompute(k)\n\t}\n}\n"},
		{"go-loop-query-call", "go", "lib.go",
			"package x\nfunc f(db *DB, ids []int) {\n\tfor _, id := range ids {\n\t\tdb.Query(id)\n\t}\n}\n",
			"package x\nfunc f(db *DB, id int) {\n\tdb.Query(id)\n}\n"},

		// --- Python -----------------------------------------------------
		{"py-mutable-default-arg", "python", "lib.py",
			"def f(x=[]):\n    return x\n",
			"def f(x=None):\n    return x\n"},
		{"py-self-comparison", "python", "lib.py",
			"if a == a:\n    pass\n",
			"if a == b:\n    pass\n"},
		{"py-check-then-act-dict", "python", "lib.py",
			"if k not in d:\n    d[k] = 1\n",
			"if k not in d:\n    compute(k)\n"},
		{"py-loop-query-call", "python", "lib.py",
			"for u in users:\n    db.query(u)\n",
			"db.query(u)\n"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			bad := runDetector(t, c.name, c.lang, c.file, c.bad)
			require.GreaterOrEqual(t, bad.Total, 1,
				"review rule %q should fire on its positive fixture; got 0", c.name)
			require.Equal(t, CategoryReview, lookupCategory(c.name),
				"review rule %q must carry CategoryReview", c.name)

			good := runDetector(t, c.name, c.lang, c.file, c.good)
			require.Equal(t, 0, good.Total,
				"review rule %q should be silent on its negative fixture; got %d", c.name, good.Total)
		})
	}
}

// TestReviewRulesCoverBothLanguages asserts the rulepack ships at
// least one review detector for each of Go and Python (the languages
// the spec requires).
func TestReviewRulesCoverBothLanguages(t *testing.T) {
	langs := map[string]int{}
	for _, info := range DescribeDetectors() {
		if info.Category != CategoryReview {
			continue
		}
		for _, l := range info.Languages {
			langs[l]++
		}
	}
	require.GreaterOrEqual(t, langs["go"], 1, "expected at least one Go review rule")
	require.GreaterOrEqual(t, langs["python"], 1, "expected at least one Python review rule")
}

func lookupCategory(name string) string {
	d, ok := lookupDetector(name)
	if !ok {
		return ""
	}
	return d.Category
}
