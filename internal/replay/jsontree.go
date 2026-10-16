package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
)

// node is a JSON value that remembers the order of its object members and the
// spelling of its numbers. A replayed frame is the recorded one with a few
// values swapped, and decoding it into maps would hand the server its members
// sorted and its numbers respelled, which is a different frame for no reason.
type node struct {
	kind    nodeKind
	str     string      // a string's value
	num     json.Number // a number as it was written
	boolean bool
	items   []*node  // an array's elements
	members []member // an object's members, in the order they arrived
}

type member struct {
	key   string
	value *node
}

type nodeKind int

const (
	nullNode nodeKind = iota
	boolNode
	numberNode
	stringNode
	arrayNode
	objectNode
)

// parseNode decodes one JSON value. Repeated member names are kept as they
// came, since a frame that carries them is the frame to replay.
func parseNode(raw []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := readNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after a JSON value")
	}
	return n, nil
}

func readNode(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case nil:
		return &node{kind: nullNode}, nil
	case bool:
		return &node{kind: boolNode, boolean: v}, nil
	case json.Number:
		return &node{kind: numberNode, num: v}, nil
	case string:
		return &node{kind: stringNode, str: v}, nil
	case json.Delim:
		switch v {
		case '[':
			n := &node{kind: arrayNode}
			for dec.More() {
				item, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, item)
			}
			_, err := dec.Token() // ]
			return n, err
		case '{':
			n := &node{kind: objectNode}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				value, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.members = append(n.members, member{key: key, value: value})
			}
			_, err := dec.Token() // }
			return n, err
		}
	}
	return nil, errors.New("unexpected JSON token")
}

// get returns the value of an object's first member named key.
func (n *node) get(key string) *node {
	if n == nil || n.kind != objectNode {
		return nil
	}
	for _, m := range n.members {
		if m.key == key {
			return m.value
		}
	}
	return nil
}

// encode writes the value compactly, with strings escaped the way mcpsnoop
// writes captured bytes everywhere else.
func (n *node) encode() []byte {
	var buf bytes.Buffer
	n.writeTo(&buf)
	return buf.Bytes()
}

func (n *node) writeTo(buf *bytes.Buffer) {
	switch n.kind {
	case nullNode:
		buf.WriteString("null")
	case boolNode:
		if n.boolean {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case numberNode:
		buf.WriteString(n.num.String())
	case stringNode:
		writeString(buf, n.str)
	case arrayNode:
		buf.WriteByte('[')
		for i, item := range n.items {
			if i > 0 {
				buf.WriteByte(',')
			}
			item.writeTo(buf)
		}
		buf.WriteByte(']')
	case objectNode:
		buf.WriteByte('{')
		for i, m := range n.members {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, m.key)
			buf.WriteByte(':')
			m.value.writeTo(buf)
		}
		buf.WriteByte('}')
	}
}

func writeString(buf *bytes.Buffer, s string) {
	// A string always marshals, so the error is only there for the interface.
	quoted, _ := jsonwire.Marshal(s)
	buf.Write(quoted)
}
