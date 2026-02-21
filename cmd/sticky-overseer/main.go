package main

import (
	overseer "github.com/whisper-darkly/sticky-overseer/v2"
	_ "github.com/whisper-darkly/sticky-overseer/v2/exec" // built-in exec handler
)

// version and commit are injected at build time via -ldflags.
// External consumers can follow this same pattern with their own handlers:
//
//	import (
//	    overseer "github.com/whisper-darkly/sticky-overseer/v2"
//	    _ "github.com/whisper-darkly/sticky-overseer/v2/exec"  // built-in handler
//	    _ "myorg/handlers/docker"                              // custom handler
//	)
//	func main() { overseer.RunCLI(version, commit) }
var version = "dev"
var commit = "unknown"

func main() { overseer.RunCLI(version, commit) }
