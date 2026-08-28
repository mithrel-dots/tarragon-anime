package mpv

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Stream struct {
	URL      string
	Headers  map[string]string
	Subtitle string
}

type Player struct {
	args      []string
	configDir string
	logger    *log.Logger
	timeout   time.Duration
}

func New(args []string, logger *log.Logger) *Player {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Player{
		args: append([]string(nil), args...), configDir: standardConfigDir(),
		logger: logger, timeout: 12 * time.Second,
	}
}

func (p *Player) Play(ctx context.Context, stream Stream, title string) error {
	socket, err := socketPath()
	if err != nil {
		return fmt.Errorf("create mpv IPC path: %w", err)
	}
	args := append([]string(nil), p.args...)
	args = append(args,
		"--config=yes",
		"--config-dir="+p.configDir,
		"--no-terminal",
		"--input-ipc-server="+socket,
		"--force-media-title="+title,
	)
	if len(stream.Headers) > 0 {
		keys := make([]string, 0, len(stream.Headers))
		for key := range stream.Headers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fields := make([]string, 0, len(keys))
		for _, key := range keys {
			fields = append(fields, key+": "+stream.Headers[key])
		}
		args = append(args, "--http-header-fields="+strings.Join(fields, ","))
	}
	if stream.Subtitle != "" {
		args = append(args, "--sub-file="+stream.Subtitle)
	}
	args = append(args, "--", stream.URL)

	cmd := exec.Command("mpv", args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = p.logger.Writer()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mpv: %w", err)
	}
	p.logger.Printf("mpv launched pid=%d config_dir=%q title=%q", cmd.Process.Pid, p.configDir, title)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	conn, err := p.connect(ctx, socket, exited)
	if err != nil {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		default:
		}
		_ = os.Remove(socket)
		return fmt.Errorf("connect mpv IPC: %w", err)
	}

	ready := make(chan error, 1)
	go p.ownIPC(conn, ready)
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = os.Remove(socket)
			return fmt.Errorf("initialize mpv IPC: %w", err)
		}
		go func() {
			err := <-exited
			if err != nil {
				p.logger.Printf("mpv exited: %v", err)
			}
			_ = conn.Close()
			_ = os.Remove(socket)
		}()
		return nil
	case err := <-exited:
		_ = conn.Close()
		_ = os.Remove(socket)
		if err == nil {
			return fmt.Errorf("mpv exited before IPC initialization")
		}
		return fmt.Errorf("mpv exited before IPC initialization: %w", err)
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = conn.Close()
		_ = os.Remove(socket)
		return ctx.Err()
	case <-time.After(p.timeout):
		_ = cmd.Process.Kill()
		_ = conn.Close()
		_ = os.Remove(socket)
		return fmt.Errorf("timed out waiting for mpv IPC response")
	}
}

func (p *Player) connect(ctx context.Context, socket string, exited <-chan error) (net.Conn, error) {
	deadline := time.NewTimer(p.timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-exited:
			if err == nil {
				return nil, fmt.Errorf("mpv exited before opening its IPC socket")
			}
			return nil, err
		case <-deadline.C:
			return nil, fmt.Errorf("timed out waiting for socket %s", socket)
		case <-ticker.C:
		}
	}
}

func (p *Player) ownIPC(conn net.Conn, ready chan<- error) {
	request := map[string]any{"command": []any{"get_property", "path"}, "request_id": 1}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		ready <- err
		return
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	sentReady := false
	for scanner.Scan() {
		var message struct {
			Event     string `json:"event"`
			Error     string `json:"error"`
			RequestID int    `json:"request_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if message.RequestID == 1 && !sentReady {
			sentReady = true
			if message.Error != "success" {
				ready <- fmt.Errorf("mpv rejected IPC verification: %s", message.Error)
				return
			}
			ready <- nil
		}
		switch message.Event {
		case "file-loaded":
			p.logger.Printf("mpv event=file-loaded")
		case "end-file", "shutdown":
			p.logger.Printf("mpv event=%s", message.Event)
		}
	}
	if !sentReady {
		if err := scanner.Err(); err != nil {
			ready <- err
		} else {
			ready <- fmt.Errorf("mpv closed IPC before verification")
		}
	}
}

func socketPath() (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return filepath.Join(os.TempDir(), "tarragon-anime-mpv-"+hex.EncodeToString(random)+".sock"), nil
}

func standardConfigDir() string {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "mpv")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "mpv")
	}
	return ""
}
