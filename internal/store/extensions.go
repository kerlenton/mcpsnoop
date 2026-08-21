package store

import (
	"encoding/json"
	"slices"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// ExtensionsCapability is the capabilities field carrying the negotiated
// extension map (SEP-2133). Keys are reverse-DNS extension ids and values are
// per-extension settings, which this screen deliberately does not interpret.
// Exported so the renderer can skip it when listing capability names: it is a
// container of extension ids, not a capability of its own.
const ExtensionsCapability = "extensions"

// extensionIDs returns the extension ids one side advertised, sorted. It is a
// no-op on capabilities carrying no extensions map, and on a malformed one,
// since a declaration we cannot read is not evidence that an extension is in
// play, and guessing here would be worse than saying nothing.
func extensionIDs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var caps struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(raw, &caps) != nil || len(caps.Extensions) == 0 {
		return nil
	}
	ids := make([]string, 0, len(caps.Extensions))
	for id := range caps.Extensions {
		ids = append(ids, id)
	}
	slices.Sort(ids) // map iteration order is not stable
	return ids
}

// mergeExtensions folds both sides into one list keyed by id. The union is the
// right shape because agreement is a property of the pair, not of a side: an
// extension only one side advertised has to stay visible, since "advertised but
// not in play" is exactly what explains a feature silently doing nothing.
func mergeExtensions(client, server []string) []ExtensionView {
	if len(client) == 0 && len(server) == 0 {
		return nil
	}
	inClient := make(map[string]bool, len(client))
	for _, id := range client {
		inClient[id] = true
	}
	inServer := make(map[string]bool, len(server))
	for _, id := range server {
		inServer[id] = true
	}

	ids := make([]string, 0, len(inClient)+len(inServer))
	ids = append(ids, client...)
	for _, id := range server {
		if !inClient[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)

	out := make([]ExtensionView, 0, len(ids))
	for _, id := range ids {
		out = append(out, ExtensionView{ID: id, Client: inClient[id], Server: inServer[id]})
	}
	return out
}

// tasksExtension is the identifier the Tasks extension is negotiated under. It
// is the one extension mcpsnoop models the methods of, so it is the one it can
// say anything about.
const tasksExtension = "io.modelcontextprotocol/tasks"

// extensionsFrom is the revision that moved optional features out of the core
// protocol and into negotiated extensions (SEP-2133).
//
// The gate is not cosmetic. In 2025-11-25 tasks/get, tasks/result, tasks/cancel
// and tasks/list are in the core schema and there is no extensions field to
// negotiate, so a client driving them there is correct and reporting it would
// fire on conforming traffic. In 2026-07-28 they are gone from the core schema
// and Tasks is an extension like any other.
const extensionsFrom = "2026-07-28"

// extensionState is what a capture can say about one side's support for one
// extension. Three-valued on purpose: a declaration mcpsnoop never saw, or one
// its own redaction scrubbed, is not the same as a side that declared none, and
// treating the two alike turns a capture that starts mid-session into an
// accusation.
type extensionState int

const (
	extensionUnknown extensionState = iota
	extensionAbsent
	extensionAdvertised
)

// advertises reports whether one side's capabilities declare an extension.
// Capabilities that were never observed, or that will not decode, answer
// unknown rather than absent.
func advertises(raw json.RawMessage, id string) extensionState {
	if len(raw) == 0 {
		return extensionUnknown
	}
	var caps struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(raw, &caps) != nil {
		return extensionUnknown
	}
	if _, ok := caps.Extensions[id]; ok {
		return extensionAdvertised
	}
	return extensionAbsent
}

// unnegotiatedTasksWarning reports a frame driving the Tasks extension at a side
// that never advertised it.
//
// The rule is in Versioning and Compatibility for 2026-07-28, "If one party
// supports an extension but the other does not, the supporting party MUST either
// revert to core protocol behavior or reject the request with an appropriate
// error". Neither happened here: the feature was used anyway, and what a reader
// gets instead is a -32601 or a -32021 several frames later, or a task that
// simply never progresses.
//
// dir is the direction of the frame using the extension, so the side that has to
// have advertised it is the other one. Silence is the answer whenever the
// capture cannot show that side's declaration, since a capture that starts after
// the handshake is the ordinary case rather than a violation.
func (sess *session) unnegotiatedTasksWarning(dir proxy.Direction, version string) string {
	if !atLeastRevision(valueOrDefault(version, sess.caps.protocolVersion), extensionsFrom) {
		return ""
	}
	peer, side := sess.caps.server, "server"
	if dir == proxy.ServerToClient {
		peer, side = sess.caps.client, "client"
	}
	if advertises(peer, tasksExtension) != extensionAbsent {
		return ""
	}
	return "uses the " + tasksExtension + " extension, which the " + side +
		" never advertised, and 2026-07-28 requires the supporting side to fall back to core behaviour or reject the request"
}

// valueOrDefault prefers a request's own declared revision and falls back to the
// session's, so a stateless capture where every request carries its version is
// judged by that request rather than by whatever the handshake said.
func valueOrDefault(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
