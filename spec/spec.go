// Package spec embeds the normative kitbash specifications so the daemon
// validates against the same files the repository publishes.
package spec

import _ "embed"

// ManifestSchema is spec/manifest.schema.json, the JSON Schema 2020-12
// definition of kitbash.yaml.
//
//go:embed manifest.schema.json
var ManifestSchema []byte

// ManifestSchemaURL is the $id of ManifestSchema.
const ManifestSchemaURL = "https://kitbash.zyx.tw/spec/manifest.schema.json"
