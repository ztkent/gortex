package graph

import "testing"

func TestEnclosingFromID(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		kind     NodeKind
		wantID   string
		wantName string
	}{
		{
			name:     "method on type",
			id:       "pkg/decoder.go::Decoder.Parse",
			kind:     KindMethod,
			wantID:   "pkg/decoder.go::Decoder",
			wantName: "Decoder",
		},
		{
			name:     "field of struct",
			id:       "pkg/config.go::Config.Timeout",
			kind:     KindField,
			wantID:   "pkg/config.go::Config",
			wantName: "Config",
		},
		{
			name:     "enum member",
			id:       "pkg/kind.go::NodeKind.Function",
			kind:     KindEnumMember,
			wantID:   "pkg/kind.go::NodeKind",
			wantName: "NodeKind",
		},
		{
			name:     "closure inside function",
			id:       "pkg/run.go::Execute#closure@42",
			kind:     KindClosure,
			wantID:   "pkg/run.go::Execute",
			wantName: "Execute",
		},
		{
			name:     "nested type method",
			id:       "pkg/x.go::Outer.Inner.Do",
			kind:     KindMethod,
			wantID:   "pkg/x.go::Outer.Inner",
			wantName: "Inner",
		},
		{
			name:     "top-level function -- no owner",
			id:       "pkg/x.go::TopLevel",
			kind:     KindFunction,
			wantID:   "",
			wantName: "",
		},
		{
			name:     "plain method ID without owner segment",
			id:       "pkg/x.go::Bare",
			kind:     KindMethod,
			wantID:   "",
			wantName: "",
		},
		{
			name:     "type is never enclosed",
			id:       "pkg/x.go::SomeType",
			kind:     KindType,
			wantID:   "",
			wantName: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotName := EnclosingFromID(tc.id, tc.kind)
			if gotID != tc.wantID || gotName != tc.wantName {
				t.Fatalf("EnclosingFromID(%q, %v) = (%q, %q), want (%q, %q)",
					tc.id, tc.kind, gotID, gotName, tc.wantID, tc.wantName)
			}
		})
	}
}

func TestNodeBrief_Enclosing(t *testing.T) {
	// A method node surfaces its receiver type in Brief.
	method := &Node{ID: "pkg/d.go::Decoder.Parse", Kind: KindMethod, Name: "Parse", FilePath: "pkg/d.go"}
	b := method.Brief()
	if b["enclosing"] != "Decoder" {
		t.Errorf("method Brief enclosing = %v, want Decoder", b["enclosing"])
	}
	if b["enclosing_id"] != "pkg/d.go::Decoder" {
		t.Errorf("method Brief enclosing_id = %v, want pkg/d.go::Decoder", b["enclosing_id"])
	}

	// A top-level function has no enclosing key at all.
	fn := &Node{ID: "pkg/d.go::TopLevel", Kind: KindFunction, Name: "TopLevel", FilePath: "pkg/d.go"}
	fb := fn.Brief()
	if _, ok := fb["enclosing"]; ok {
		t.Error("top-level function Brief should carry no enclosing key")
	}
}

func TestEnclosingShortName(t *testing.T) {
	cases := map[string]string{
		"pkg/x.go::Owner": "Owner",
		"Outer.Inner":     "Inner",
		"Plain":           "Plain",
		"pkg::A.B.C":      "C",
	}
	for in, want := range cases {
		if got := EnclosingShortName(in); got != want {
			t.Errorf("EnclosingShortName(%q) = %q, want %q", in, got, want)
		}
	}
}
