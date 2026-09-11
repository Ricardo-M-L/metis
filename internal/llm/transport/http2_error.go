package transport

import "reflect"

// retryableHTTP2StreamError recognizes the two transient stream resets before
// provider redaction replaces the original error with the safe ErrNetwork
// marker. net/http's private http2StreamError does not implement net.Error or
// Unwrap; importing x/net/http2 and using errors.As would miss that real type.
// Inspect only these exact Go package/type identities and exported numeric
// fields. Do not match arbitrary error text or infer retryability from Cause.
func retryableHTTP2StreamError(err error) bool {
	if err == nil {
		return false
	}
	v := reflect.ValueOf(err)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	t := v.Type()
	knownType := t.PkgPath() == "net/http" && t.Name() == "http2StreamError" ||
		t.PkgPath() == "golang.org/x/net/http2" && t.Name() == "StreamError"
	if knownType && v.Kind() == reflect.Struct {
		code, stream := v.FieldByName("Code"), v.FieldByName("StreamID")
		if code.IsValid() && code.Kind() == reflect.Uint32 && stream.IsValid() && stream.Kind() == reflect.Uint32 && stream.Uint() > 0 {
			// HTTP/2 INTERNAL_ERROR and REFUSED_STREAM. CANCEL is deliberately
			// excluded, as are protocol, security and unknown error codes.
			return code.Uint() == 0x2 || code.Uint() == 0x7
		}
		return false
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if retryableHTTP2StreamError(child) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return retryableHTTP2StreamError(wrapped.Unwrap())
	}
	return false
}
