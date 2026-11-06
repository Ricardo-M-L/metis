package transcript

import (
	"reflect"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// DeepSnapshot detaches mutable message blocks and JSON-like metadata for an
// accepted provider request. Strings (including image data) remain immutable
// shared values; no JSON round-trip drops host-only fields or changes numbers.
func DeepSnapshot(messages []llm.Message) []llm.Message {
	if messages == nil {
		return nil
	}
	out := make([]llm.Message, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].Content = snapshotBlocks(message.Content)
	}
	return out
}

func snapshotBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := make([]llm.ContentBlock, len(blocks))
	for i, block := range blocks {
		out[i] = block
		out[i].ToolResultBlocks = snapshotBlocks(block.ToolResultBlocks)
		if block.ToolInput != nil {
			out[i].ToolInput = cloneMetadata(reflect.ValueOf(block.ToolInput)).Interface().(map[string]any)
		}
		if block.Presentation != nil {
			out[i].Presentation = cloneMetadata(reflect.ValueOf(block.Presentation)).Interface().(map[string]any)
		}
		if block.ProviderHint != nil {
			out[i].ProviderHint = make(map[string]string, len(block.ProviderHint))
			for key, value := range block.ProviderHint {
				out[i].ProviderHint[key] = value
			}
		}
	}
	return out
}

// Tool arguments/presentation are acyclic JSON-like values. Preserve concrete
// slice/map types, unlike serialization-based cloning; scalar values are safe.
func cloneMetadata(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.New(value.Type()).Elem()
		out.Set(cloneMetadata(value.Elem()))
		return out
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), cloneMetadata(iter.Value()))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneMetadata(value.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneMetadata(value.Index(i)))
		}
		return out
	default:
		return value
	}
}
