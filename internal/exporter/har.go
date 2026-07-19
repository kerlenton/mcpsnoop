package exporter

import (
	"encoding/json"
	"io"
	"time"
)

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

func WriteHAR(w io.Writer, data SessionExport) error {
	har := harRoot{
		Log: harLog{
			Version: "1.2",
			Creator: harCreator{
				Name:    "mcpsnoop",
				Version: "dev",
			},
			Entries: []harEntry{},
		},
	}
	for _, call := range data.Calls {
		entry := harEntry{
			StartedDateTime: call.StartedAt,
			Request: harRequest{
				Method:      "POST",
				URL:         "mcp://" + call.Method,
				HTTPVersion: "JSON-RPC 2.0",
				Headers:     []harHeader{},
				QueryString: []harQueryParam{},
				Cookies:     []harCookie{},
				HeadersSize: -1,
				BodySize:    len(call.Params),
				PostData: &harPostData{
					MimeType: "application/json",
					Text:     string(call.Params),
				},
			},
			Response: harResponse{
				Status:      200,
				StatusText:  call.Status,
				HTTPVersion: "JSON-RPC 2.0",
				Headers:     []harHeader{},
				Cookies:     []harCookie{},
				HeadersSize: -1,
				BodySize:    len(call.Result),
				Content: harContent{
					Size:     len(call.Result),
					MimeType: "application/json",
					Text:     string(call.Result),
				},
			},
		}

		if call.DurationMS != nil {
			entry.Time = *call.DurationMS
		}

		har.Log.Entries = append(har.Log.Entries, entry)
	}

	return json.NewEncoder(w).Encode(har)
}
