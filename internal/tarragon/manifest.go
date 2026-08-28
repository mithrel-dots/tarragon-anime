package tarragon

const (
	Version  = "0.1.0"
	Manifest = `id = "anime"
version = "` + Version + `"
name = "Anime"
description = "Search and watch anime"
enabled = true
entrypoint = "tarragon-anime"
lifecycle_mode = "daemon"
prefix = "anime"
require_prefix = true
provides_general_suggestions = false
capabilities = ["suggest", "icon"]
build_dependencies = ["go", "mpv"]
`
)
