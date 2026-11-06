// Package githubagentproxy contains resources embedded in the proxy binary.
package githubagentproxy

import _ "embed"

// ThirdPartyNotices contains the licenses of bundled dependencies.
//
//go:embed THIRD_PARTY_NOTICES.md
var ThirdPartyNotices string
