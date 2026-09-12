package otlp

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Lengths of the two identifiers, in hex characters.
const (
	traceIDHexLen = 32
	spanIDHexLen  = 16
)

// idFields are the JSON names of every trace or span identifier in the three
// signals, in both the camel case and the underscore spelling protojson
// accepts. Links, exemplars and log records reuse the same names, so walking
// the document by field name catches all of them.
var idFields = map[string]int{
	"traceId":        traceIDHexLen,
	"trace_id":       traceIDHexLen,
	"spanId":         spanIDHexLen,
	"span_id":        spanIDHexLen,
	"parentSpanId":   spanIDHexLen,
	"parent_span_id": spanIDHexLen,
}

// hexIdentifiers rewrites the identifiers of an OTLP JSON body from hex to
// base64.
//
// The OTLP specification says trace_id and span_id are hex strings in JSON,
// but protojson reads every bytes field as base64, so a conforming producer
// would land in the store with mangled identifiers. Bodies that already carry
// base64 are left alone: a hex identifier is exactly 32 or 16 hex characters,
// and base64 of the same bytes is 24 or 12 characters, so the two never
// collide. That keeps kitbashd compatible with both the specification and the
// protojson based exporters that are common in the field.
//
// Numbers are kept verbatim rather than through float64, because a timestamp
// in unix nanoseconds does not survive that round trip.
func hexIdentifiers(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("otlp: body is not JSON: %w", err)
	}
	if !rewrite(doc) {
		return body, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("otlp: rewrite identifiers: %w", err)
	}
	return out, nil
}

// rewrite converts every hex identifier in place and reports whether it
// changed anything, so an untouched body is passed on as it arrived.
func rewrite(node any) bool {
	changed := false
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if want, isID := idFields[key]; isID {
				s, ok := child.(string)
				if !ok || len(s) != want {
					continue
				}
				raw, err := hex.DecodeString(s)
				if err != nil {
					continue
				}
				value[key] = base64.StdEncoding.EncodeToString(raw)
				changed = true
				continue
			}
			if rewrite(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range value {
			if rewrite(child) {
				changed = true
			}
		}
	}
	return changed
}
