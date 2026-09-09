PREFIX ?= $(HOME)/.local
PLUGIN_DIR ?= $(PREFIX)/lib/tarragon/plugins/anime
DESKTOP_DIR ?= $(PREFIX)/share/applications
DESKTOP_FILE ?= $(DESKTOP_DIR)/tarragon-anime.desktop

.PHONY: check check-deps install uninstall run test-live test-race

check:
	@files="$$(gofmt -l .)"; if [ -n "$$files" ]; then printf '%s\n' "unformatted files:" "$$files" >&2; exit 1; fi
	go mod tidy -diff
	go mod verify
	go build ./...
	go vet ./...
	go test ./...

test-race:
	go test -race ./...

# Opt-in integration tests that require network access to AniList and AllAnime.
# Browser tests need Chromium and network access; AllAnime opens a visible
# clearance window when needed. ANIME_LIVE_FRESH=1 uses a temporary profile for
# AllAnime. ANIME_LIVE_CLEARANCE=1 also enables the local real-Chromium
# clearance/restart regression test (requires a display).
test-live:
	go test -tags=live -timeout 20m -p 1 ./internal/anilist ./internal/browser ./internal/provider/allanime

check-deps:
	@command -v go >/dev/null || { printf '%s\n' 'go is required' >&2; exit 1; }
	@command -v mpv >/dev/null || { printf '%s\n' 'mpv is required' >&2; exit 1; }
	@command -v chromium >/dev/null || printf '%s\n' 'chromium not found: stream resolution will use the native fallback' >&2

install: check-deps
	install -d "$(PLUGIN_DIR)" "$(DESKTOP_DIR)"
	go build -o "$(PLUGIN_DIR)/tarragon-anime" ./cmd/anime
	install -m 0644 plugin.toml "$(PLUGIN_DIR)/plugin.toml"
	printf '%s\n' \
		'[Desktop Entry]' \
		'Type=Application' \
		'Name=Tarragon Anime AniList Login' \
		'Comment=Receives the AniList OAuth callback' \
		'Exec=$(PLUGIN_DIR)/tarragon-anime auth %u' \
		'Terminal=false' \
		'NoDisplay=true' \
		'MimeType=x-scheme-handler/tarragon-anime;' \
		> "$(DESKTOP_FILE)"
	@command -v update-desktop-database >/dev/null && update-desktop-database "$(DESKTOP_DIR)" || true
	@command -v xdg-mime >/dev/null && xdg-mime default tarragon-anime.desktop x-scheme-handler/tarragon-anime || true

uninstall:
	rm -rf "$(PLUGIN_DIR)"
	rm -f "$(DESKTOP_FILE)"
	@command -v update-desktop-database >/dev/null && update-desktop-database "$(DESKTOP_DIR)" || true

run: check-deps
	go run ./cmd/anime
