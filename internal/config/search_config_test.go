package config

import "testing"

func TestEffectiveKeywordSoupRewrite(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", KeywordSoupSplit},
		{"split", KeywordSoupSplit},
		{"SPLIT", KeywordSoupSplit},
		{" split ", KeywordSoupSplit},
		{"nudge", KeywordSoupNudge},
		{"Nudge", KeywordSoupNudge},
		{"off", KeywordSoupOff},
		{"OFF", KeywordSoupOff},
		{"bogus", KeywordSoupSplit}, // unknown folds to the safe default
	}
	for _, tc := range cases {
		got := SearchConfig{KeywordSoupRewrite: tc.in}.EffectiveKeywordSoupRewrite()
		if got != tc.want {
			t.Errorf("EffectiveKeywordSoupRewrite(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDefaultSkipSearch_KeepsProseDropsCodeBlocks(t *testing.T) {
	rules := DefaultSkipSearch()
	// The noisy markdown code-block `variable` nodes are still skipped.
	if !ShouldSkipSearch(rules, "markdown", "variable") {
		t.Error("markdown variable (code-block) nodes should still be skipped from search")
	}
	// The new KindDoc prose-section nodes must NOT be skipped -- they
	// are the searchable documentation corpus.
	if ShouldSkipSearch(rules, "markdown", "doc") {
		t.Error("markdown doc (prose-section) nodes must stay IN the search index")
	}
}

func TestIndexProseEnabled(t *testing.T) {
	// Unset (nil pointer) defaults to enabled.
	if !(SearchConfig{}).IndexProseEnabled() {
		t.Error("IndexProse should default to enabled when unset")
	}
	on, off := true, false
	if !(SearchConfig{IndexProse: &on}).IndexProseEnabled() {
		t.Error("IndexProse:true should be enabled")
	}
	if (SearchConfig{IndexProse: &off}).IndexProseEnabled() {
		t.Error("IndexProse:false should be disabled")
	}
}

func TestEquivalenceClassesEnabled(t *testing.T) {
	if !(SearchConfig{}).EquivalenceClassesEnabled() {
		t.Error("EquivalenceClasses should default to enabled when unset")
	}
	off := false
	if (SearchConfig{EquivalenceClasses: &off}).EquivalenceClassesEnabled() {
		t.Error("EquivalenceClasses:false should be disabled")
	}
}

func TestDefault_IndexProseOn(t *testing.T) {
	// config.Default() must seed Index.IndexProse true so non-manager
	// callers (single-shot CLI paths) index prose by default.
	if !Default().Index.IndexProse {
		t.Error("Default().Index.IndexProse should be true")
	}
}
