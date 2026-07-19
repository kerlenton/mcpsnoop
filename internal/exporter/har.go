package exporter

import (
	"encoding/json"
	"io"
	"time"
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
func WriteHAR(w io.Writer, data SessionExport) error {
	label := data.Session.Label
	if label == "" {
		label = data.Session.ID
	}

	entries := make([]harEntry, 0, len(data.Calls))
	for _, call := range data.Calls {
		status, statusText := harStatus(call.Status)

		// Only the total round trip is known, so it all lands in wait, and time is
		// the sum of the three. An unanswered call has no round trip, so it stays 0.
		var durationMS float64
		if call.DurationMS != nil {
			durationMS = *call.DurationMS
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
			Timings: harTimings{Send: 0, Wait: durationMS, Receive: 0},
		})
	}

	payload := harRoot{Log: harLog{
		Version: "1.2",
		Creator: harCreator{Name: "mcpsnoop", Version: Version},
		Entries: entries,
	}}
	enc := json.NewEncoder(w)
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
		if encoded, err := json.Marshal(call.Error); err == nil {
			return string(encoded)
		}
	}
	return ""
}
