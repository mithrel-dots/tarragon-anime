PREFIX ?= $(HOME)/.local
PLUGIN_DIR ?= $(PREFIX)/lib/tarragon/plugins/anime

.PHONY: check-deps install uninstall run

check-deps:
	@command -v go >/dev/null || { printf '%s\n' 'go is required' >&2; exit 1; }
	@command -v mpv >/dev/null || { printf '%s\n' 'mpv is required' >&2; exit 1; }

install: check-deps
	install -d "$(PLUGIN_DIR)"
	go build -o "$(PLUGIN_DIR)/tarragon-anime" ./cmd/anime
	install -m 0644 plugin.toml "$(PLUGIN_DIR)/plugin.toml"

uninstall:
	rm -rf "$(PLUGIN_DIR)"

run: check-deps
	go run ./cmd/anime
