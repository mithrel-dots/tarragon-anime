PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
PLUGIN_DIR ?= $(XDG_CONFIG_HOME)/tarragon/plugins/anime
ifeq ($(XDG_CONFIG_HOME),)
PLUGIN_DIR := $(HOME)/.config/tarragon/plugins/anime
endif

.PHONY: check-deps install uninstall run

check-deps:
	@command -v go >/dev/null || { printf '%s\n' 'go is required' >&2; exit 1; }
	@command -v mpv >/dev/null || { printf '%s\n' 'mpv is required' >&2; exit 1; }

install: check-deps
	install -d "$(BINDIR)" "$(PLUGIN_DIR)"
	go build -o "$(BINDIR)/tarragon-anime" ./cmd/anime
	install -m 0644 plugin.toml "$(PLUGIN_DIR)/plugin.toml"

uninstall:
	rm -f "$(BINDIR)/tarragon-anime"
	rm -rf "$(PLUGIN_DIR)"

run: check-deps
	go run ./cmd/anime
