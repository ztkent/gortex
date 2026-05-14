package svc

import (
	"reflect"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

func TestAssistCache_GetSetEvict(t *testing.T) {
	c := newAssistCache(2)

	c.Set("a", []string{"1"})
	c.Set("b", []string{"2"})
	if got, ok := c.Get("a"); !ok || !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("get a: ok=%v got=%v", ok, got)
	}

	// Insert beyond cap; oldest ("a") must evict.
	c.Set("c", []string{"3"})
	if _, ok := c.Get("a"); ok {
		t.Fatalf("expected a to be evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatalf("expected b to remain")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatalf("expected c to be present")
	}
}

func TestAssistCache_UpdateInPlace(t *testing.T) {
	c := newAssistCache(2)
	c.Set("a", []string{"1"})
	c.Set("b", []string{"2"})
	// Update "a" — must NOT count as a fresh insert that would
	// evict "b".
	c.Set("a", []string{"1", "1b"})
	if got, _ := c.Get("a"); !reflect.DeepEqual(got, []string{"1", "1b"}) {
		t.Fatalf("update lost: %v", got)
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatalf("update should not evict; b missing")
	}
}

func TestAssistCache_CopyOnReadAndWrite(t *testing.T) {
	c := newAssistCache(2)
	val := []string{"x", "y"}
	c.Set("k", val)

	// Mutate caller-side input post-Set; cache must be unaffected.
	val[0] = "MUTATED"
	got, _ := c.Get("k")
	if got[0] != "x" {
		t.Fatalf("cache mirrored caller mutation: %v", got)
	}

	// Mutate returned slice; subsequent Get must still return the
	// original.
	got[0] = "ALSO_MUTATED"
	got2, _ := c.Get("k")
	if got2[0] != "x" {
		t.Fatalf("cache returns aliased slice: %v", got2)
	}
}

func TestAssistCache_Concurrent(t *testing.T) {
	c := newAssistCache(128)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + (i % 26)))
			c.Set(key, []string{key})
			c.Get(key)
		}(i)
	}
	wg.Wait()
}

func TestRerankCacheKey_StableAcrossOrder(t *testing.T) {
	a := []llm.RerankCandidate{{ID: "x"}, {ID: "y"}, {ID: "z"}}
	b := []llm.RerankCandidate{{ID: "z"}, {ID: "x"}, {ID: "y"}}
	if rerankCacheKey("q", a) != rerankCacheKey("q", b) {
		t.Fatalf("key must be independent of input ordering")
	}
}

func TestRerankCacheKey_DiffersOnQuery(t *testing.T) {
	c := []llm.RerankCandidate{{ID: "x"}}
	if rerankCacheKey("q1", c) == rerankCacheKey("q2", c) {
		t.Fatalf("different queries must produce different keys")
	}
}

func TestParseStringList(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		key  string
		want []string
	}{
		{"happy", `{"terms":["a","b","c"]}`, "terms", []string{"a", "b", "c"}},
		{"empty array", `{"terms":[]}`, "terms", nil},
		{"missing key", `{"order":["a"]}`, "terms", nil},
		{"malformed", `{terms:[a]}`, "terms", nil},
		{"non-array value", `{"terms":"oops"}`, "terms", nil},
		{"blank input", ``, "terms", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStringList(tc.raw, tc.key)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestDedupeFilter(t *testing.T) {
	got := dedupeFilter([]string{"BCrypt", "bcrypt", "", "  ", "argon2", "validate"}, "validate")
	want := []string{"BCrypt", "argon2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestDedupeFilter_DropsStoplist(t *testing.T) {
	got := dedupeFilter(
		[]string{"function", "library", "bcrypt", "data", "argon2", "general"},
		"hash passwords")
	want := []string{"bcrypt", "argon2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestDedupeFilter_DropsShortTerms(t *testing.T) {
	got := dedupeFilter([]string{"jwt", "is", "do", "id", "bcrypt", "ab"}, "auth")
	want := []string{"jwt", "bcrypt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestDedupeFilter_RespectsMaxCap(t *testing.T) {
	in := []string{"a1234", "b1234", "c1234", "d1234", "e1234", "f1234", "g1234"}
	got := dedupeFilter(in, "q")
	if len(got) != maxExpansionTerms {
		t.Fatalf("len=%d want=%d", len(got), maxExpansionTerms)
	}
}

func TestDedupeFilter_StoplistCaseInsensitive(t *testing.T) {
	got := dedupeFilter([]string{"FUNCTION", "Library", "BCrypt"}, "q")
	want := []string{"BCrypt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestFilterToInputAppend(t *testing.T) {
	cands := []llm.RerankCandidate{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	model := []string{"c", "hallucinated", "a", "c"} // dup + hallucinated
	got := filterToInputAppend(model, cands)
	want := []string{"c", "a", "b", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestFilterToInputAppend_ModelEmpty(t *testing.T) {
	cands := []llm.RerankCandidate{{ID: "a"}, {ID: "b"}}
	got := filterToInputAppend(nil, cands)
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestVerifyIDs(t *testing.T) {
	if got := verifyIDs(nil); got != nil {
		t.Fatalf("nil input: got=%v", got)
	}
	in := []llm.VerifyCandidate{{ID: "a"}, {ID: "b"}}
	got := verifyIDs(in)
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestFilterKeepToInput(t *testing.T) {
	cands := []llm.VerifyCandidate{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	model := []string{"c", "hallucinated", "a", "c"} // dup + hallucinated
	got := filterKeepToInput(model, cands)
	want := []string{"c", "a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestFilterKeepToInput_EmptyKeepHonoured(t *testing.T) {
	// Critical contract: an empty model result must produce an empty
	// keep list. Dropped IDs MUST NOT be appended back.
	cands := []llm.VerifyCandidate{{ID: "a"}, {ID: "b"}}
	got := filterKeepToInput(nil, cands)
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %v", got)
	}
}

func TestFilterKeepToInput_AllHallucinated(t *testing.T) {
	cands := []llm.VerifyCandidate{{ID: "a"}, {ID: "b"}}
	got := filterKeepToInput([]string{"x", "y"}, cands)
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %v", got)
	}
}

func TestVerifyCacheKey_DiffersWhenBodyChanges(t *testing.T) {
	a := []llm.VerifyCandidate{{ID: "x", Body: "old code"}}
	b := []llm.VerifyCandidate{{ID: "x", Body: "new code"}}
	if verifyCacheKey("q", a) == verifyCacheKey("q", b) {
		t.Fatalf("body change must produce different cache key")
	}
}

func TestVerifyCacheKey_StableAcrossOrder(t *testing.T) {
	a := []llm.VerifyCandidate{{ID: "x", Body: "X"}, {ID: "y", Body: "Y"}}
	b := []llm.VerifyCandidate{{ID: "y", Body: "Y"}, {ID: "x", Body: "X"}}
	if verifyCacheKey("q", a) != verifyCacheKey("q", b) {
		t.Fatalf("key must be independent of input ordering")
	}
}

func TestCandIDs(t *testing.T) {
	if got := candIDs(nil); got != nil {
		t.Fatalf("nil input: got=%v", got)
	}
	in := []llm.RerankCandidate{{ID: "z"}, {ID: "a"}}
	got := candIDs(in)
	want := []string{"z", "a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}
