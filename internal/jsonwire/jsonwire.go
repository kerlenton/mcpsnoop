// Package jsonwire encodes JSON that has to keep matching bytes somebody else
// sent.
//
// encoding/json escapes <, > and & as \u003c, \u003e and \u0026 by
// default. That is a safety default for JSON embedded in HTML, and it is wrong
// for a wire capture: a tool result containing markup is stored as something
// the server never sent, and Raw is the one field an envelope promises is
// verbatim. The
// escaping also applies to the contents of a json.RawMessage, so it reaches
// payloads that are only being passed through.
//
// Use this wherever captured bytes are written. Do not use it where the output
// is embedded in HTML: there the escaping is what keeps a payload containing
// </script> inside its script block, and writeHTML in the exporter depends on
// it.
package jsonwire

import (
	"bytes"
	"encoding/json"
	"io"
)

// NewEncoder returns an encoder that leaves <, > and & alone. The caller may
// still set indentation.
func NewEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

// Marshal is json.Marshal without the escaping, and without the trailing
// newline json.Encoder.Encode appends.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
