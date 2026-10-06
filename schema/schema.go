// Package schema embeds the pinned ACP JSON schemas that acp/v1 and acp/v2 are generated from.
package schema

import _ "embed"

//go:embed v1/schema.json
var V1 []byte

//go:embed v2/schema.json
var V2 []byte
