package exporter

import (
	"fmt"
	"io"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
)

// Version is recorded as the HAR creator version. It mirrors tui.Version and can
// be set from main at startup. The OTLP exporter omits its version entirely, but
// HAR requires creator.version, so this defaults to the same "dev" main uses.
var Version = "dev"

// HAR 1.2, see http://www.softwareishard.com/blog/har-12-spec/. Three rules from
// the spec drive the shapes below. An entry must carry cache and timings, they
// are not optional. entry.time must equal the sum of the timings that are not -1.
// send, wait and receive are themselves not optional and must be non-negative,
// so -1 is only legal for blocked, dns, connect and ssl, which are omitted here.
type harRoot struct {
	Log harLog `json:"log"`
}

type harLog struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Entries []harEntry `json:"entries"`
	// Comment is optional on log in HAR 1.2 and is where an incomplete capture
	// says so. Unlike the OTLP attribute this is prose for a person reading the
	// file, so it appears only when there is something to say; a note reading
	// "nothing was dropped" on every export would be noise in a field that
	// devtools render as a remark.
	Comment string `json:"comment,omitempty"`
}

type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type harEntry struct {
	StartedDateTime time.Time   `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
	Cache           harCache    `json:"cache"`
	Timings         harTimings  `json:"timings"`
}

type harRequest struct {
	Method      string          `json:"method"`
	URL         string          `json:"url"`
	HTTPVersion string          `json:"httpVersion"`
	Headers     []harHeader     `json:"headers"`
	QueryString []harQueryParam `json:"queryString"`
	Cookies     []harCookie     `json:"cookies"`
	HeadersSize int             `json:"headersSize"`
	BodySize    int             `json:"bodySize"`
	PostData    *harPostData    `json:"postData,omitempty"`
}

type harResponse struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Headers     []harHeader `json:"headers"`
	Cookies     []harCookie `json:"cookies"`
	Content     harContent  `json:"content"`
	RedirectURL string      `json:"redirectURL"`
	HeadersSize int         `json:"headersSize"`
	BodySize    int         `json:"bodySize"`
}

// harCache is required on every entry. mcpsnoop observes a live exchange, so
// there is never cache information to report and it stays an empty object.
type harCache struct{}

type harTimings struct {
	// Blocked is time the entry spent waiting on something other than the server.
	// Under multi round-trip requests that is the client gathering an answer from
	// a person, which is what the field is for, and putting it in wait drew a
	// server that had been busy for the whole interaction. A one hop call reports
	// zero, since nothing blocked it, and -1, which HAR 1.2 reserves for a timing
	// that does not apply, is used only when there is no split to report at all.
	Blocked float64 `json:"blocked"`
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

type harHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harQueryParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harPostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

type harContent struct {
	Size     int    `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
}

// harHTTPVersion labels the protocol in the viewer's version column. MCP is not
// HTTP, so this names what actually spoke rather than pretending otherwise.
const harHTTPVersion = "JSON-RPC/2.0"

// WriteHAR renders a session as HAR 1.2, one entry per correlated call, so a
// capture can be opened in the browser devtools and other tools that read HAR.
//
// When a capture is incomplete the entry count understates what happened on the
// wire, so the file says so in log.comment. It belongs there rather than in this
// doc comment: the person who needs it opened the HAR, not this file.
func WriteHAR(w io.Writer, data SessionExport) error {
	label := data.Session.Label
	if label == "" {
		label = data.Session.ID
	}

	entries := make([]harEntry, 0, len(data.Calls))
	for _, call := range data.Calls {
		status, statusText := harStatus(call.Status)

		// time is the whole interaction, split between the server and whatever the
		// client was doing between hops. An unanswered call has no round trip, so it
		// stays 0.
		var durationMS float64
		if call.DurationMS != nil {
			durationMS = *call.DurationMS
		}
		// wait is the server's share and blocked is the rest. Without the split a
		// viewer drew a thirty-eight second server wait for an operation the server
		// spent one second on, with the other thirty-seven belonging to a person
		// answering an elicitation. An operation with no measured share reports -1,
		// which is how HAR 1.2 spells a timing that does not apply.
		// The split needs a duration large enough to contain the server's share. An
		// operation with no exportable duration, which is every pending, superseded
		// and cancelled one, has zero here, so subtracting a server share that is
		// real produced a negative blocked. HAR 1.2 allows exactly one negative,
		// the -1 that means the timing does not apply, so that is what such an entry
		// reports.
		wait, blocked := durationMS, float64(-1)
		if call.ServerTimeMS != nil && *call.ServerTimeMS <= durationMS {
			wait = *call.ServerTimeMS
			blocked = durationMS - wait
		}

		request := harRequest{
			Method:      "POST",
			URL:         harURL(label, call.Method, call.ToolName),
			HTTPVersion: harHTTPVersion,
			Headers:     []harHeader{},
			QueryString: []harQueryParam{},
			Cookies:     []harCookie{},
			HeadersSize: -1, // there are no headers to measure, so the size is unknown
			BodySize:    len(call.Params),
		}
		if len(call.Params) > 0 {
			request.PostData = &harPostData{MimeType: "application/json", Text: string(call.Params)}
		}

		body := harResponseBody(call)
		entries = append(entries, harEntry{
			StartedDateTime: call.StartedAt,
			Time:            durationMS,
			Request:         request,
			Response: harResponse{
				Status:      status,
				StatusText:  statusText,
				HTTPVersion: harHTTPVersion,
				Headers:     []harHeader{},
				Cookies:     []harCookie{},
				Content:     harContent{Size: len(body), MimeType: "application/json", Text: body},
				RedirectURL: "",
				HeadersSize: -1,
				BodySize:    len(body),
			},
			Cache:   harCache{},
			Timings: harTimings{Blocked: blocked, Send: 0, Wait: wait, Receive: 0},
		})
	}

	payload := harRoot{Log: harLog{
		Version: "1.2",
		Creator: harCreator{Name: "mcpsnoop", Version: Version},
		Entries: entries,
		Comment: harIncompleteComment(data.Session.MissingFrames),
	}}
	enc := jsonwire.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// harURL synthesises a URL for a protocol that has none. stdio sessions have no
// address at all, so the server label and the operation stand in for one. The
// tool name is appended because otherwise every tools/call shares a URL, which
// is the one column a HAR viewer is scanned by.
func harURL(label, method, toolName string) string {
	url := "mcp://" + label + "/" + method
	if toolName != "" {
		url += "/" + toolName
	}
	return url
}

// harStatus maps a call outcome onto an HTTP status a viewer can colour. MCP
// carries its errors inside an otherwise successful response, so this is a lens
// rather than a transcript: 200 for a call that succeeded, 500 for one that
// failed, and 0 for one that never got a response, which is what HAR uses for a
// request that produced none.
func harStatus(status string) (int, string) {
	switch status {
	case "ok":
		return 200, "OK"
	case "error":
		return 500, "Error"
	case "late_result":
		return 200, "Late Result"
	case "call_cancelled":
		return 0, "Cancelled"
	case "":
		return 0, "No Response"
	default:
		return 0, status // pending or superseded, never answered
	}
}

// harResponseBody prefers the result, falling back to the JSON-RPC error object
// so a failed call still shows why in the viewer's response pane.
func harResponseBody(call CallExport) string {
	if len(call.Result) > 0 {
		return string(call.Result)
	}
	if call.Error != nil {
		// jsonwire, like the encoder above. This branch flattens the error object
		// into a string field, so escaping here would leave one HAR file carrying
		// two encodings of content.text depending on whether the call failed, and
		// RPCError.Data is verbatim wire bytes.
		if encoded, err := jsonwire.Marshal(call.Error); err == nil {
			return string(encoded)
		}
	}
	return ""
}

// harIncompleteComment describes a lossy capture for whoever opens the file, or
// returns "" when nothing was dropped.
//
// The wording says what the reader can and cannot conclude, because the risk
// here is not that entries look wrong but that they look complete: a call whose
// frames never reached the log is simply absent, and absence reads as "it never
// happened" rather than "we did not see it".
func harIncompleteComment(missing uint64) string {
	if missing == 0 {
		return ""
	}
	frames := "frames"
	if missing == 1 {
		frames = "frame"
	}
	return fmt.Sprintf(
		"mcpsnoop: capture incomplete, %d %s were dropped before being recorded. "+
			"The entry count is a lower bound, and a call missing from this file may "+
			"have happened on the wire.", missing, frames)
}
