// Package manifest parses, interpolates, validates, and translates the
// pmcluster DSL (pkg/dsl) into a Docker Swarm Compose YAML suitable for
// `docker stack deploy`.
//
// Stages, in order:
//   1. Parse        — YAML bytes → *dsl.App, strict (unknown keys = error)
//   2. Interpolate  — substitute ${app}/${env}/${version}/${registry}/${env:VAR}
//   3. Validate     — semantic checks (image required, expose.port valid, etc.)
//   4. Translate    — *dsl.App → compose YAML []byte (translate.go)
//
// Each stage is a separate function so the deploy service (internal/deploy)
// can run them in turn and stash the source/rendered pair in SQLite.
package manifest

import (
	"bytes"
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
)

// Parse decodes a manifest YAML document into a *dsl.App.
//
// Strict mode: unknown keys cause an error (sigs.k8s.io/yaml's
// YAMLToJSONStrict + json.Decoder.DisallowUnknownFields). This catches typos
// like `repalicas: 2` early instead of silently dropping the value.
//
// Returns the parsed App without interpolation or validation — the caller
// must run Interpolate then Validate.
func Parse(data []byte) (*dsl.App, error) {
	jsonData, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	var app dsl.App
	dec := jsonDecoder(bytes.NewReader(jsonData))
	if err := dec.Decode(&app); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &app, nil
}
