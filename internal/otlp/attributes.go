package otlp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"

	commonpb "github.com/zyx1121/kitbash/internal/otlpproto/common/v1"

	"github.com/zyx1121/kitbash/internal/store"
)

// The wire names of the kitbash attributes, see PLAN.md section 2.4. The query
// surface speaks their short names; these are what a producer sends.
const (
	AttrUser     = "kitbash.user"
	AttrPackage  = "kitbash.package"
	AttrProcess  = "kitbash.process"
	AttrUnit     = "kitbash.unit"
	AttrPath     = "kitbash.path"
	AttrTool     = "kitbash.tool"
	AttrEval     = "kitbash.eval"
	AttrProducer = "kitbash.producer"
	AttrCaller   = "kitbash.caller"
	AttrInternal = "kitbash.internal"
)

// The subject of an evaluation record: the span a judgment is about, see
// PLAN.md section 2.4. Both stay in Other, because nothing filters on them and
// a typed column would have to be indexed to earn its place.
const (
	AttrSubjectTraceID = "kitbash.subject.trace_id"
	AttrSubjectSpanID  = "kitbash.subject.span_id"
)

// attributes merges the resource attributes of a record with its own, the
// record winning, and lifts the kitbash attributes into typed columns. A
// kitbash attribute of the wrong type stays in Other rather than being coerced:
// the store would otherwise answer a query with a value the producer never
// sent.
//
// kitbash.producer is lifted like the rest and then overwritten by the
// receiver, so a producer naming itself something else changes nothing.
// kitbash.caller is lifted like the rest and then resolved by the receiver:
// what a producer sends is a credential kitbashd minted for one session, which
// the receiver rewrites to the Process id or drops. Nothing that arrives here
// is trusted to name a Process, see PLAN.md section 2.3.
func attributes(resource map[string]any, record []*commonpb.KeyValue) store.Attributes {
	merged := make(map[string]any, len(resource)+len(record))
	for k, v := range resource {
		merged[k] = v
	}
	for k, v := range keyValues(record) {
		merged[k] = v
	}

	var attrs store.Attributes
	for key, target := range map[string]*string{
		AttrUser:     &attrs.User,
		AttrPackage:  &attrs.Package,
		AttrProcess:  &attrs.Process,
		AttrUnit:     &attrs.Unit,
		AttrPath:     &attrs.Path,
		AttrTool:     &attrs.Tool,
		AttrProducer: &attrs.Producer,
		AttrCaller:   &attrs.Caller,
	} {
		if s, ok := merged[key].(string); ok {
			*target = s
			delete(merged, key)
		}
	}
	// kitbash.eval decides how a record is stamped and whether a query for
	// judgments returns it, so a value of the wrong type is dropped rather
	// than left among the other attributes: a string "true" sitting under the
	// eval name is a value a reader would take for the flag it is not.
	if raw, sent := merged[AttrEval]; sent {
		if b, ok := raw.(bool); ok {
			attrs.Eval = &b
		}
		delete(merged, AttrEval)
	}
	// kitbash.internal decides who may read the record at all, so it is
	// lifted the same way and a value of the wrong type is dropped rather
	// than kept: a record carrying the string "true" under this name would
	// be answered to every member as an ordinary record.
	if raw, sent := merged[AttrInternal]; sent {
		if b, ok := raw.(bool); ok {
			attrs.Internal = &b
		}
		delete(merged, AttrInternal)
	}
	if len(merged) > 0 {
		attrs.Other = merged
	}
	return attrs
}

// keyValues turns a list of OTLP attributes into a plain map.
func keyValues(kvs []*commonpb.KeyValue) map[string]any {
	if len(kvs) == 0 {
		return nil
	}
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		if kv.GetKey() == "" {
			continue
		}
		// A value with no JSON spelling, such as NaN, is left out entirely
		// rather than stored as null.
		converted := value(kv.GetValue())
		if converted == nil {
			continue
		}
		out[kv.GetKey()] = converted
	}
	return out
}

// value converts an OTLP AnyValue into something encoding/json can store.
// Bytes become base64, the same shape the OTLP JSON encoding uses.
func value(v *commonpb.AnyValue) any {
	switch kind := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return kind.StringValue
	case *commonpb.AnyValue_BoolValue:
		return kind.BoolValue
	case *commonpb.AnyValue_IntValue:
		return kind.IntValue
	case *commonpb.AnyValue_DoubleValue:
		// NaN and infinity have no JSON spelling, so an attribute carrying
		// one is dropped and the record is kept without it. Storing it would
		// make every later query of that window unanswerable, which costs
		// every member the answer for one producer's bug.
		if math.IsNaN(kind.DoubleValue) || math.IsInf(kind.DoubleValue, 0) {
			return nil
		}
		return kind.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(kind.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		list := make([]any, 0, len(kind.ArrayValue.GetValues()))
		for _, item := range kind.ArrayValue.GetValues() {
			list = append(list, value(item))
		}
		return list
	case *commonpb.AnyValue_KvlistValue:
		return keyValues(kind.KvlistValue.GetValues())
	default:
		return nil
	}
}

// text renders a log body as the single string the logRecord schema promises.
// A structured body is kept as its JSON rather than dropped.
func text(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	if s, ok := v.GetValue().(*commonpb.AnyValue_StringValue); ok {
		return s.StringValue
	}
	converted := value(v)
	if converted == nil {
		return ""
	}
	b, err := json.Marshal(converted)
	if err != nil {
		return fmt.Sprint(converted)
	}
	return string(b)
}
