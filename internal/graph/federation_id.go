package graph

import "strings"

// IsProxyNode reports whether n is a federation Option-B proxy node —
// identified by its struct fields, NOT its ID shape. Distinct from
// IsStub(id) (the stdlib/builtin/module string predicate): a proxy node
// is keyed under the "remote:<slug>~..." origin namespace, which IsStub
// does not recognise. Proxy nodes are excluded from graph_stats / BM25 /
// communities / analyzers and never persisted to the durable store.
func IsProxyNode(n *Node) bool {
	return n != nil && n.Stub && n.Origin != ""
}

// proxyIDPrefix is the origin-namespace marker for a proxy node id.
const proxyIDPrefix = "remote:"

// proxyIDSep separates the origin segment from the remote's native id.
const proxyIDSep = "~"

// ProxyNodeID composes the origin-namespaced id for a remote symbol so a
// remote node can never alias a local id, even when two daemons share a
// repo prefix: "remote:<slug>~<remoteRepoPrefix>/<file>::<sym>".
func ProxyNodeID(slug, remoteID string) string {
	return proxyIDPrefix + slug + proxyIDSep + remoteID
}

// IsProxyID reports whether id is an origin-namespaced proxy id.
func IsProxyID(id string) bool {
	if !strings.HasPrefix(id, proxyIDPrefix) {
		return false
	}
	return strings.Contains(id[len(proxyIDPrefix):], proxyIDSep)
}

// ProxyOriginSlug returns the <slug> of a proxy id, or "" if id is not a
// proxy id.
func ProxyOriginSlug(id string) string {
	if !strings.HasPrefix(id, proxyIDPrefix) {
		return ""
	}
	rest := id[len(proxyIDPrefix):]
	i := strings.Index(rest, proxyIDSep)
	if i < 0 {
		return ""
	}
	return rest[:i]
}

// ProxyRemoteID returns the remote's native id encoded in a proxy id, or
// "" if id is not a proxy id.
func ProxyRemoteID(id string) string {
	if !strings.HasPrefix(id, proxyIDPrefix) {
		return ""
	}
	rest := id[len(proxyIDPrefix):]
	i := strings.Index(rest, proxyIDSep)
	if i < 0 {
		return ""
	}
	return rest[i+len(proxyIDSep):]
}
