package githubagentproxy

import _ "embed"

// AgentSkill describes how to discover and use the running proxy's policy.
//
//go:embed skills/github-agent-proxy/SKILL.md
var AgentSkill string
