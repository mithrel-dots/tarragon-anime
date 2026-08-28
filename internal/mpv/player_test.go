package mpv

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlayerRequiresAndUsesIPC(t *testing.T) {
	dir := t.TempDir()
	configHome := filepath.Join(dir, "config")
	script := filepath.Join(dir, "mpv")
	content := "#!/bin/sh\nexec \"$MPV_TEST_BINARY\" -test.run=TestMPVHelperProcess -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args")
	commandsFile := filepath.Join(dir, "commands")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MPV_TEST_BINARY", os.Args[0])
	t.Setenv("MPV_HELPER", "1")
	t.Setenv("MPV_ARGS_FILE", argsFile)
	t.Setenv("MPV_COMMANDS_FILE", commandsFile)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	player := New([]string{"--profile=anime"}, log.New(io.Discard, "", 0))
	player.timeout = 2 * time.Second
	session, err := player.Play(t.Context(), Stream{
		URL: "https://video.test/master.m3u8", Headers: map[string]string{"Referer": "https://mkissa.to/"},
		Subtitle: "https://sub.test/en.ass",
	}, "Frieren - Episode 4", 15)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Load(t.Context(), Stream{URL: "https://video.test/next.m3u8"}, "Frieren - Episode 5", 0); err != nil {
		t.Fatal(err)
	}
	if err := session.AddSubtitle(t.Context(), "https://sub.test/next.ass"); err != nil {
		t.Fatal(err)
	}
	if err := session.ShowText(t.Context(), "Episode 5"); err != nil {
		t.Fatal(err)
	}
	session.Close()
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(args)
	for _, expected := range []string{
		"--config=yes",
		"--config-dir=" + filepath.Join(configHome, "mpv"),
		"--profile=anime",
		"--idle=yes",
		"--start=15.000",
		"--input-ipc-server=",
		"--http-header-fields=Referer: https://mkissa.to/",
		"--sub-file=https://sub.test/en.ass",
		"https://video.test/master.m3u8",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("mpv args %q do not contain %q", joined, expected)
		}
	}
	commands, err := os.ReadFile(commandsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"define-section", "Shift+N", "Shift+P", "loadfile", "next.m3u8", "sub-add", "show-text"} {
		if !strings.Contains(string(commands), expected) {
			t.Errorf("mpv commands %q do not contain %q", commands, expected)
		}
	}
}

func TestMPVHelperProcess(t *testing.T) {
	if os.Getenv("MPV_HELPER") != "1" {
		return
	}
	separator := 0
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index + 1
			break
		}
	}
	args := os.Args[separator:]
	_ = os.WriteFile(os.Getenv("MPV_ARGS_FILE"), []byte(strings.Join(args, "\n")), 0o600)
	socket := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "--input-ipc-server=") {
			socket = strings.TrimPrefix(arg, "--input-ipc-server=")
		}
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(2)
	}
	conn, err := listener.Accept()
	if err != nil {
		os.Exit(3)
	}
	scanner := bufio.NewScanner(conn)
	encoder := json.NewEncoder(conn)
	for scanner.Scan() {
		var request struct {
			Command   []any `json:"command"`
			RequestID int   `json:"request_id"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(5)
		}
		file, _ := os.OpenFile(os.Getenv("MPV_COMMANDS_FILE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = file.Write(append(append([]byte(nil), scanner.Bytes()...), '\n'))
		_ = file.Close()
		_ = encoder.Encode(map[string]any{"request_id": request.RequestID, "error": "success", "data": "ok"})
		if len(request.Command) > 0 && request.Command[0] == "quit" {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	_ = conn.Close()
	_ = listener.Close()
	os.Exit(0)
}
