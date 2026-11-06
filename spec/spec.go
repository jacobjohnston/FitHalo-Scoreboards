// Package spec embeds the machine-readable parts of the API specification, so the server enforces
// exactly the files the clients read.
package spec

import _ "embed"

// Sharing is spec/sharing.json: all weekly challenge data and the sharing it needs.
//
//go:embed sharing.json
var Sharing []byte
