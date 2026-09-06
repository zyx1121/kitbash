package otlp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/zyx1121/kitbash/internal/store"
)

// The wire names of the six kitbash attributes, see PLAN.md section 2.4. The
// query surface speaks their short names; these are what a producer sends.
const (
	AttrUser    = "kitbash.user"
	AttrPackage = "kitbash.package"
	AttrProcess = "kitbash.process"
	AttrPath    = "kitbash.path"
	AttrTool    = "kitbash.tool"
	AttrEval    = "kitbash.eval"
)

// attributes merges the resource attributes of a record with its own, the
// record winning, and lifts the six kitbash attributes into typed columns. A
// kitbash attribute of the wrong type stays in Other rather than being coerced:
// the store would otherwise answer a query with a value the producer never
// sent.
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
		AttrUser:    &attrs.User,
		AttrPackage: &attrs.Package,
		AttrProcess: &attrs.Process,
		AttrPath:    &attrs.Path,
		AttrTool:    &attrs.Tool,
	} {
		if s, ok := merged[key].(string); ok {
			*target = s
			delete(merged, key)
		}
	}
	if b, ok := merged[AttrEval].(bool); ok {
		attrs.Eval = &b
		delete(merged, AttrEval)
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
		out[kv.GetKey()] = value(kv.GetValue())
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
