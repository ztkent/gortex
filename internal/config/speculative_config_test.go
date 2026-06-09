package config

import "testing"

func TestSpeculativeDispatchEnabledOrDefault(t *testing.T) {
	var ic IndexConfig
	if ic.SpeculativeDispatchEnabledOrDefault() {
		t.Errorf("speculative dispatch must default OFF when unset")
	}
	tt := true
	ic.SynthesizeSpeculativeDispatch = &tt
	if !ic.SpeculativeDispatchEnabledOrDefault() {
		t.Errorf("explicit true must enable")
	}
	ff := false
	ic.SynthesizeSpeculativeDispatch = &ff
	if ic.SpeculativeDispatchEnabledOrDefault() {
		t.Errorf("explicit false must disable")
	}
}
