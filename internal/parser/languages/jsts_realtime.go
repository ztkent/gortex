package languages

import "regexp"

// Real-time transport edge extraction (WebSocket + Server-Sent Events).
// A `new WebSocket(url)` or `new EventSource(url)` is the client side of a
// real-time stream; modelling it as a subscribe edge onto a shared
// KindEvent channel node (transport "websocket" / "sse") lets
// `analyze kind=pubsub` / `event_emitters` and architecture queries see
// the real-time surface the call graph otherwise misses. These transports
// are cross-process, so the in-process event-channel synthesizer
// deliberately does not pair them.

const (
	transportWebSocket = "websocket"
	transportSSE       = "sse"
)

var (
	// new WebSocket(<url-or-var>) — captures the first argument token,
	// the channel URL (string literal) or the variable holding it.
	jsWebSocketCtorRe = regexp.MustCompile("new\\s+WebSocket\\s*\\(\\s*[`'\"]?([^`'\",)\\s]*)")
	// new EventSource(<url>) — also the ReconnectingEventSource polyfill.
	jsEventSourceCtorRe = regexp.MustCompile("new\\s+(?:Reconnecting)?EventSource\\s*\\(\\s*[`'\"]?([^`'\",)\\s]*)")
)

// detectJSRealtimeEvents scans a JS/TS source for WebSocket / EventSource
// client constructors and returns one subscribe pubsubEvent per site,
// ready to flow through emitPubsubEvents alongside the broker pub/sub
// events. The channel topic is the URL literal when present, else the
// variable name, else "connection".
func detectJSRealtimeEvents(src []byte) []pubsubEvent {
	s := string(src)
	var out []pubsubEvent
	collect := func(transport string, re *regexp.Regexp) {
		for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
			topic := "connection"
			if m[2] >= 0 && m[3] > m[2] {
				if arg := s[m[2]:m[3]]; arg != "" {
					topic = arg
				}
			}
			out = append(out, pubsubEvent{
				role:      pubsubRoleSubscribe,
				transport: transport,
				topic:     topic,
				method:    "new " + transport,
				line:      lineAt(src, m[0]),
			})
		}
	}
	collect(transportWebSocket, jsWebSocketCtorRe)
	collect(transportSSE, jsEventSourceCtorRe)
	return out
}
