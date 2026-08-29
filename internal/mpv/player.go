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
	"sync"
	"time"
)

type Stream struct {
	URL      string
	Headers  map[string]string
	Subtitle string
}

type EventType int

const (
	EventFileLoaded EventType = iota
	EventEndFile
	EventShutdown
	EventNext
	EventPrevious
	EventPosition
	EventDuration
	EventSkip
)

type Event struct {
	Type   EventType
	Reason string
	Value  float64
}

type Player struct {
	args      []string
	configDir string
	logger    *log.Logger
	timeout   time.Duration
}

type SessionController interface {
	Events() <-chan Event
	Load(context.Context, Stream, string, float64) error
	AddSubtitle(context.Context, string) error
	ShowText(context.Context, string) error
	Close()
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

func (p *Player) Play(ctx context.Context, stream Stream, title string, start float64) (SessionController, error) {
	socket, err := socketPath()
	if err != nil {
		return nil, fmt.Errorf("create mpv IPC path: %w", err)
	}
	args := append([]string(nil), p.args...)
	args = append(args,
		"--config=yes",
		"--config-dir="+p.configDir,
		"--no-terminal",
		"--idle=yes",
		"--force-window=yes",
		"--input-ipc-server="+socket,
		"--force-media-title="+title,
	)
	if start > 0 {
		args = append(args, fmt.Sprintf("--start=%.3f", start))
	}
	if fields := headerFields(stream.Headers); len(fields) > 0 {
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
		return nil, fmt.Errorf("start mpv: %w", err)
	}
	p.logger.Printf("mpv launched pid=%d config_dir=%q title=%q", cmd.Process.Pid, p.configDir, title)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	conn, err := p.connect(ctx, socket, exited)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(socket)
		return nil, fmt.Errorf("connect mpv IPC: %w", err)
	}
	session := newSession(conn, cmd, socket, p.logger)
	go session.readIPC()
	go session.wait(exited)

	if _, err := session.command(ctx, "get_property", "path"); err != nil {
		session.kill()
		return nil, fmt.Errorf("initialize mpv IPC: %w", err)
	}
	commands := [][]any{
		{"observe_property", 1, "time-pos"},
		{"observe_property", 2, "duration"},
		{"define-section", "tarragon-anime", "Shift+N script-message tarragon-next\nShift+P script-message tarragon-previous\nShift+S script-message tarragon-skip", "force"},
		{"enable-section", "tarragon-anime"},
	}
	for _, command := range commands {
		if _, err := session.command(ctx, command...); err != nil {
			session.kill()
			return nil, fmt.Errorf("configure mpv IPC: %w", err)
		}
	}
	return session, nil
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

type Session struct {
	conn   net.Conn
	cmd    *exec.Cmd
	socket string
	logger *log.Logger
	events chan Event
	done   chan struct{}

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int
	pending map[int]chan ipcResponse
	once    sync.Once
}

type ipcResponse struct {
	data json.RawMessage
	err  error
}

func newSession(conn net.Conn, cmd *exec.Cmd, socket string, logger *log.Logger) *Session {
	return &Session{
		conn: conn, cmd: cmd, socket: socket, logger: logger,
		events: make(chan Event, 64), done: make(chan struct{}),
		pending: make(map[int]chan ipcResponse),
	}
}

func (s *Session) Events() <-chan Event {
	return s.events
}

func (s *Session) Load(ctx context.Context, stream Stream, title string, start float64) error {
	if _, err := s.command(ctx, "set_property", "http-header-fields", headerFields(stream.Headers)); err != nil {
		return err
	}
	if _, err := s.command(ctx, "set_property", "force-media-title", title); err != nil {
		return err
	}
	if _, err := s.command(ctx, "set_property", "start", fmt.Sprintf("%.3f", start)); err != nil {
		return err
	}
	if _, err := s.command(ctx, "loadfile", stream.URL, "replace"); err != nil {
		return err
	}
	return nil
}

func (s *Session) AddSubtitle(ctx context.Context, subtitle string) error {
	if subtitle == "" {
		return nil
	}
	_, err := s.command(ctx, "sub-add", subtitle, "select")
	return err
}

func (s *Session) ShowText(ctx context.Context, message string) error {
	_, err := s.command(ctx, "show-text", message, 3000)
	return err
}

func (s *Session) Seek(ctx context.Context, position float64) error {
	_, err := s.command(ctx, "seek", position, "absolute+exact")
	return err
}

func (s *Session) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.command(ctx, "quit"); err != nil {
		s.kill()
	}
}

func (s *Session) command(ctx context.Context, args ...any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	response := make(chan ipcResponse, 1)
	s.pending[id] = response
	s.mu.Unlock()

	message := map[string]any{"command": args, "request_id": id}
	s.writeMu.Lock()
	err := json.NewEncoder(s.conn).Encode(message)
	s.writeMu.Unlock()
	if err != nil {
		s.removePending(id)
		return nil, err
	}
	select {
	case result := <-response:
		return result.data, result.err
	case <-s.done:
		s.removePending(id)
		return nil, fmt.Errorf("mpv IPC closed")
	case <-ctx.Done():
		s.removePending(id)
		return nil, ctx.Err()
	}
}

func (s *Session) readIPC() {
	scanner := bufio.NewScanner(s.conn)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var message struct {
			Event     string          `json:"event"`
			Error     string          `json:"error"`
			RequestID int             `json:"request_id"`
			Data      json.RawMessage `json:"data"`
			Name      string          `json:"name"`
			Reason    string          `json:"reason"`
			Args      []string        `json:"args"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if message.RequestID != 0 {
			s.mu.Lock()
			response := s.pending[message.RequestID]
			delete(s.pending, message.RequestID)
			s.mu.Unlock()
			if response != nil {
				var err error
				if message.Error != "success" {
					err = fmt.Errorf("mpv command failed: %s", message.Error)
				}
				response <- ipcResponse{data: message.Data, err: err}
			}
		}
		s.handleEvent(message.Event, message.Name, message.Reason, message.Args, message.Data)
	}
	s.finish()
}

func (s *Session) handleEvent(event, name, reason string, args []string, data json.RawMessage) {
	switch event {
	case "file-loaded":
		s.logger.Printf("mpv event=file-loaded")
		s.emit(Event{Type: EventFileLoaded})
	case "end-file":
		s.logger.Printf("mpv event=end-file reason=%s", reason)
		s.emit(Event{Type: EventEndFile, Reason: reason})
	case "shutdown":
		s.logger.Printf("mpv event=shutdown")
		s.emit(Event{Type: EventShutdown})
	case "client-message":
		if len(args) == 0 {
			return
		}
		switch args[0] {
		case "tarragon-next":
			s.emit(Event{Type: EventNext})
		case "tarragon-previous":
			s.emit(Event{Type: EventPrevious})
		case "tarragon-skip":
			s.emit(Event{Type: EventSkip})
		}
	case "property-change":
		var value float64
		if err := json.Unmarshal(data, &value); err != nil {
			return
		}
		switch name {
		case "time-pos":
			s.emit(Event{Type: EventPosition, Value: value})
		case "duration":
			s.emit(Event{Type: EventDuration, Value: value})
		}
	}
}

func (s *Session) emit(event Event) {
	select {
	case s.events <- event:
	default:
		if event.Type != EventPosition && event.Type != EventDuration {
			s.logger.Printf("mpv event queue full type=%d", event.Type)
		}
	}
}

func (s *Session) wait(exited <-chan error) {
	err := <-exited
	if err != nil {
		s.logger.Printf("mpv exited: %v", err)
	}
	_ = s.conn.Close()
	_ = os.Remove(s.socket)
}

func (s *Session) finish() {
	s.once.Do(func() {
		close(s.done)
		s.mu.Lock()
		for id, response := range s.pending {
			response <- ipcResponse{err: fmt.Errorf("mpv IPC closed")}
			delete(s.pending, id)
		}
		s.mu.Unlock()
		close(s.events)
	})
}

func (s *Session) kill() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.conn.Close()
}

func (s *Session) removePending(id int) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func headerFields(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, key+": "+headers[key])
	}
	return fields
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
