package tarragon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
)

const maxNDJSONLineSize = 1 << 20

const maxConcurrentRequests = 4

type Handler interface {
	// Request implementations must honor ctx cancellation for prompt resource
	// release. Run returns without waiting for non-cooperative handlers.
	Request(context.Context, string, string) Payload
	Select(context.Context, Message) (bool, string)
}

type Daemon struct {
	endpoint string
	name     string
	handler  Handler
	logger   *log.Logger

	writeMu sync.Mutex
}

type requestJob struct {
	ctx     context.Context
	cancel  context.CancelFunc
	message Message
}

type requestState struct {
	mu      *sync.Mutex
	current *requestJob
}

func (s *requestState) replace(job *requestJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil {
		s.current.cancel()
	}
	s.current = job
}

func (s *requestState) finish(job *requestJob) {
	s.mu.Lock()
	if s.current == job {
		s.current = nil
	}
	s.mu.Unlock()
	job.cancel()
}

func (s *requestState) writeResponse(w io.Writer, job *requestJob, payload Payload) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != job || job.ctx.Err() != nil {
		return false, nil
	}
	err := json.NewEncoder(w).Encode(response{Type: "response", QueryID: job.message.QueryID, Data: payload})
	return err == nil, err
}

type requestQueue struct {
	mu      sync.Mutex
	ready   chan struct{}
	pending *requestJob
}

func newRequestQueue() *requestQueue {
	return &requestQueue{ready: make(chan struct{}, 1)}
}

func (q *requestQueue) replace(job *requestJob) {
	q.mu.Lock()
	q.pending = job
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *requestQueue) take() *requestJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	job := q.pending
	q.pending = nil
	return job
}

func NewDaemon(endpoint, name string, handler Handler, logger *log.Logger) *Daemon {
	return &Daemon{endpoint: endpoint, name: name, handler: handler, logger: logger}
}

func (d *Daemon) Run(ctx context.Context) error {
	conn, err := net.Dial("unix", d.endpoint)
	if err != nil {
		return fmt.Errorf("connect Tarragon endpoint %s: %w", d.endpoint, err)
	}
	defer conn.Close()
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if err := d.write(conn, Message{Type: "hello", Name: d.name}); err != nil {
		return fmt.Errorf("send Tarragon hello: %w", err)
	}
	d.logger.Printf("connected to %s", d.endpoint)

	stopClose := context.AfterFunc(runCtx, func() {
		_ = conn.Close()
	})
	defer stopClose()

	queue := newRequestQueue()
	requests := requestState{mu: &d.writeMu}
	for range maxConcurrentRequests {
		go func() {
			for {
				select {
				case <-runCtx.Done():
					return
				case <-queue.ready:
					job := queue.take()
					if job == nil {
						continue
					}
					if job.ctx.Err() != nil {
						requests.finish(job)
						continue
					}
					d.logger.Printf("request qid=%s: %s", job.message.QueryID, job.message.Text)
					payload := d.handler.Request(job.ctx, job.message.QueryID, job.message.Text)
					sent, err := requests.writeResponse(conn, job, payload)
					if err != nil && runCtx.Err() == nil {
						d.logger.Printf("write Tarragon response: %v", err)
					} else if sent {
						d.logger.Printf("response sent qid=%s", job.message.QueryID)
					}
					requests.finish(job)
				}
			}
		}()
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), maxNDJSONLineSize)
	for scanner.Scan() {
		var message Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			d.logger.Printf("invalid Tarragon message: %v", err)
			continue
		}
		switch message.Type {
		case "request":
			if message.QueryID == "" {
				continue
			}
			requestCtx, cancel := context.WithCancel(runCtx)
			job := &requestJob{ctx: requestCtx, cancel: cancel, message: message}
			requests.replace(job)
			queue.replace(job)
		case "select":
			success, text := d.handler.Select(runCtx, message)
			if err := d.write(conn, selectResponse{Type: "select_response", Success: success, Message: text}); err != nil {
				return fmt.Errorf("write Tarragon selection response: %w", err)
			}
		default:
			d.logger.Printf("unsupported Tarragon message type=%q", message.Type)
		}
	}
	if err := scanner.Err(); err != nil && runCtx.Err() == nil {
		return fmt.Errorf("read Tarragon messages: %w", err)
	}
	return nil
}

func (d *Daemon) write(conn net.Conn, message any) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return json.NewEncoder(conn).Encode(message)
}
