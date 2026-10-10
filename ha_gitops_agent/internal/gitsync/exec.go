package gitsync

import "github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"

// RunResult and Runner alias internal/execx's, so a test fake written
// against either package satisfies both. Non-zero exit is data in ExitCode; only
// a launch failure or a deadline kill is an error.
type (
	RunResult = execx.RunResult
	Runner    = execx.Runner
)
