package prompts

import _ "embed"

const RoleMarker = "<!-- trace:role-prompts:v1 -->"

//go:embed agent.md
var Common string

//go:embed coordinator.md
var Coordinator string

//go:embed worker.md
var Worker string

//go:embed recon.md
var Recon string

var Agent = Common + "\n" + Coordinator
