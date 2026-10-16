package replay

import "github.com/kerlenton/mcpsnoop/internal/proxy"

// answersAfterRequests puts a client's answer to a server request back behind
// the request, where a stdio capture recorded before 0.28.0 can have it ahead.
// Those numbered a frame once it had been forwarded and observed, so a client
// quick enough to answer in between had its answer recorded first. The plan
// matches an answer to the request before it, and sent one recorded early under
// the recorded id, to a server that had numbered its request afresh.
//
// Only an answer with no request of its id open before it moves, and only to
// just after the next request of that id, so a capture recorded in order comes
// back unchanged. Nothing else is reordered, since nothing else in a capture
// says which way it really went.
func answersAfterRequests(frames []proxy.Envelope) []proxy.Envelope {
	var (
		open    = map[string]int{} // server requests not yet answered, by id
		claimed = map[int]bool{}   // requests an early answer was moved behind
		moved   = map[int]bool{}   // early answers, by where they were recorded
		behind  = map[int][]int{}  // for a request, the early answers that follow it
	)
	for i, env := range frames {
		msg, ok := proxy.ParseRPC(env.Raw)
		if !ok || len(msg.ID) == 0 {
			continue
		}
		id := string(msg.ID)
		switch {
		case env.Direction == proxy.ServerToClient && msg.Method != "":
			if !claimed[i] {
				open[id]++
			}
		case env.Direction == proxy.ClientToServer && msg.Method == "":
			if open[id] > 0 {
				open[id]--
				continue
			}
			for j := i + 1; j < len(frames); j++ {
				if claimed[j] || frames[j].Direction != proxy.ServerToClient {
					continue
				}
				if req, ok := proxy.ParseRPC(frames[j].Raw); ok && req.Method != "" && string(req.ID) == id {
					claimed[j], moved[i] = true, true
					behind[j] = append(behind[j], i)
					break
				}
			}
		}
	}
	if len(moved) == 0 {
		return frames
	}
	out := make([]proxy.Envelope, 0, len(frames))
	for i, env := range frames {
		if moved[i] {
			continue
		}
		out = append(out, env)
		for _, k := range behind[i] {
			out = append(out, frames[k])
		}
	}
	return out
}
