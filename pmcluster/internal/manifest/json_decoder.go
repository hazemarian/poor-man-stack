package manifest

import (
	"encoding/json"
	"io"
)

// jsonDecoder returns a json.Decoder that rejects unknown fields. Pulled
// out as a tiny indirection so the parse stage can stay one-liner-clean
// and tests can swap it if we ever want to relax strictness.
func jsonDecoder(r io.Reader) *json.Decoder {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	return d
}
