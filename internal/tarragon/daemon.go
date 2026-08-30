package tarragon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
)

const maxNDJSONLineSize = 1 << 20

const maxConcurrentRequests = 4

type Handler interface {
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

	jobs := make(chan *requestJob, maxConcurrentRequests)
	var requestMu sync.Mutex
	var currentRequest *requestJob
	finishRequest := func(job *requestJob) {
		requestMu.Lock()
		if currentRequest == job {
			currentRequest = nil
		}
		requestMu.Unlock()
		job.cancel()
	}
	for range maxConcurrentRequests {
		go func() {
			for {
				select {
				case <-runCtx.Done():
					return
				case job := <-jobs:
					if job.ctx.Err() != nil {
						finishRequest(job)
						continue
					}
					d.logger.Printf("request qid=%s: %s", job.message.QueryID, job.message.Text)
					payload := d.handler.Request(job.ctx, job.message.QueryID, job.message.Text)
					if err := d.writeResponse(job.ctx, conn, response{Type: "response", QueryID: job.message.QueryID, Data: payload}); err != nil && job.ctx.Err() == nil {
						d.logger.Printf("write Tarragon response: %v", err)
					} else if job.ctx.Err() == nil {
						d.logger.Printf("response sent qid=%s", job.message.QueryID)
					}
					finishRequest(job)
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
			requestMu.Lock()
			if currentRequest != nil {
				currentRequest.cancel()
			}
			currentRequest = job
			requestMu.Unlock()
			select {
			case jobs <- job:
			case <-runCtx.Done():
				finishRequest(job)
			}
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

func (d *Daemon) writeResponse(ctx context.Context, conn net.Conn, message response) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return json.NewEncoder(conn).Encode(message)
}

func (d *Daemon) write(conn net.Conn, message any) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return json.NewEncoder(conn).Encode(message)
}
