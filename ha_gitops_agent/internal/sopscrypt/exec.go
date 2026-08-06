package sopscrypt

import "github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/execx"

// RunResult and Runner alias execx's, so Crypter.Runner stays fakeable by
// tests while this package keeps importing only stdlib-only leaves. Per
// execx: a non-zero exit is data in RunResult.ExitCode with a nil error, a
// deadline kill is an error matching context.DeadlineExceeded.
type (
	RunResult = execx.RunResult
	Runner    = execx.Runner
)
