package store

import (
	"encoding/json"
	"slices"
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
