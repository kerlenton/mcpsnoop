package proxy

import (
	"encoding/json"
	"errors"
	"io"
)

// Decode reads a stream of JSON-encoded envelopes from r and calls emit for
// each decoded envelope. io.EOF and io.ErrUnexpectedEOF are treated as clean
// ends of the input.
func Decode(r io.Reader, emit func(Envelope)) error {
	dec := json.NewDecoder(r)

	for {
		var env Envelope
		if err := dec.Decode(&env); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		emit(env)
	}
}
