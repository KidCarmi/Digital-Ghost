// Package defaults embeds default configuration files into the binary so they
// are available regardless of the working directory the binary is launched from.
package defaults

import _ "embed"

//go:embed blocklist.yaml
var Blocklist []byte
