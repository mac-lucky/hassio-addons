package gitsync

import "github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"

// RunResult and Runner alias internal/execx's, so one fake satisfies these
// and sopscrypt's same-named pair. Non-zero exit is data in ExitCode; only
// a launch failure or a deadline kill is an error.
type (
	RunResult = execx.RunResult
	Runner    = execx.Runner
)
