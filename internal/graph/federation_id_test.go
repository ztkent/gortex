package graph

import (
	"testing"
	"time"
)

// TestProxyNodeID_RoundTrip asserts the origin-namespaced id composes and
// decomposes, and that a remote whose repo prefix collides with a local
// prefix still yields a distinct (non-aliasing) proxy id.
func TestProxyNodeID_RoundTrip(t *testing.T) {
	remoteID := "gortex/internal/daemon/router.go::Router"
	pid := ProxyNodeID("r2", remoteID)
	if !IsProxyID(pid) {
		t.Fatalf("%q should be a proxy id", pid)
	}
	if got := ProxyOriginSlug(pid); got != "r2" {
		t.Errorf("origin slug = %q, want r2", got)
	}
	if got := ProxyRemoteID(pid); got != remoteID {
		t.Errorf("remote id = %q, want %q", got, remoteID)
	}
	// A local node with the same native id is never a proxy id, so no
	// alias is possible even on a shared prefix.
	if IsProxyID(remoteID) {
		t.Error("a local native id must not be mistaken for a proxy id")
	}
	if ProxyNodeID("r3", remoteID) == pid {
		t.Error("the same remote symbol under two slugs must yield distinct proxy ids")
	}
}

// TestIsProxyNode asserts proxy detection keys on the struct fields, not
// the id shape, and never confuses a stdlib stub.
func TestIsProxyNode(t *testing.T) {
	proxy := &Node{ID: ProxyNodeID("r2", "x/y.go::Z"), Origin: "remote:r2", Stub: true, FetchedAt: time.Now()}
	if !IsProxyNode(proxy) {
		t.Error("a Stub+Origin node is a proxy node")
	}
	local := &Node{ID: "x/y.go::Z"}
	if IsProxyNode(local) {
		t.Error("a local node is not a proxy node")
	}
	// Stub without Origin (some other stub convention) is not a proxy.
	if IsProxyNode(&Node{Stub: true}) {
		t.Error("Stub alone is not a proxy node")
	}
	if IsProxyNode(nil) {
		t.Error("nil is not a proxy node")
	}
	// IsStub (the string predicate) must not match a proxy id.
	if IsStub(proxy.ID) {
		t.Error("IsStub must not recognise a proxy id")
	}
}

// TestProxyHelpers_NonProxyInputs asserts the accessors are safe on
// non-proxy ids.
func TestProxyHelpers_NonProxyInputs(t *testing.T) {
	for _, id := range []string{"", "x/y.go::Z", "remote:nosep", "stdlib::fmt"} {
		if IsProxyID(id) {
			t.Errorf("%q should not be a proxy id", id)
		}
		if ProxyOriginSlug(id) != "" || ProxyRemoteID(id) != "" {
			t.Errorf("accessors should be empty for non-proxy id %q", id)
		}
	}
}
