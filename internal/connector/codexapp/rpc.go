// Package codexapp implements a dark Codex 0.152.0 app-server connector.
package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

type rpcInbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *uint64         `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type rpcResult struct {
	raw json.RawMessage
	err error
}

type rpcClient struct {
	rw                  io.ReadWriteCloser
	maxLine, maxPending int
	mu                  sync.Mutex
	writeMu             sync.Mutex
	next                uint64
	pending             map[uint64]chan rpcResult
	fatal               error
	onNotify            func(string, json.RawMessage) error
	done                chan struct{}
	closeOnce           sync.Once
}

func newRPC(rw io.ReadWriteCloser, maxLine, maxPending int, notify func(string, json.RawMessage) error) *rpcClient {
	r := &rpcClient{rw: rw, maxLine: maxLine, maxPending: maxPending, pending: make(map[uint64]chan rpcResult), onNotify: notify, done: make(chan struct{})}
	go r.readLoop()
	return r
}

func (r *rpcClient) call(ctx context.Context, method string, params any, out any) error {
	r.mu.Lock()
	if r.fatal != nil {
		err := r.fatal
		r.mu.Unlock()
		return err
	}
	if len(r.pending) >= r.maxPending {
		r.mu.Unlock()
		return fmt.Errorf("pending request limit reached")
	}
	if r.next == ^uint64(0) {
		r.mu.Unlock()
		return fmt.Errorf("request id space exhausted")
	}
	r.next++
	id := r.next
	ch := make(chan rpcResult, 1)
	r.pending[id] = ch
	r.mu.Unlock()
	b, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err == nil && len(b)+1 > r.maxLine {
		err = fmt.Errorf("outbound message exceeds line limit")
	}
	if err == nil {
		r.writeMu.Lock()
		_, err = r.rw.Write(append(b, '\n'))
		r.writeMu.Unlock()
	}
	if err != nil {
		r.remove(id)
		return fmt.Errorf("app-server transport write: %w", err)
	}
	select {
	case got := <-ch:
		if got.err != nil {
			return got.err
		}
		if out != nil && len(got.raw) != 0 {
			if err := json.Unmarshal(got.raw, out); err != nil {
				return fmt.Errorf("malformed app-server result")
			}
		}
		return nil
	case <-ctx.Done():
		r.remove(id)
		return ctx.Err()
	case <-r.done:
		r.mu.Lock()
		err := r.fatal
		r.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return err
	}
}

func (r *rpcClient) notify(method string, params any) error {
	b, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", method, params})
	if err != nil {
		return err
	}
	if len(b)+1 > r.maxLine {
		return fmt.Errorf("outbound message exceeds line limit")
	}
	r.writeMu.Lock()
	_, err = r.rw.Write(append(b, '\n'))
	r.writeMu.Unlock()
	return err
}

func (r *rpcClient) remove(id uint64) { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }

func (r *rpcClient) readLoop() {
	s := bufio.NewScanner(r.rw)
	buf := make([]byte, 0, min(r.maxLine, 64*1024))
	s.Buffer(buf, r.maxLine)
	for s.Scan() {
		var m rpcInbound
		if err := json.Unmarshal(s.Bytes(), &m); err != nil || m.JSONRPC != "2.0" {
			r.fail(errors.New("malformed app-server JSON-RPC message"))
			return
		}
		if m.ID != nil {
			r.mu.Lock()
			ch, ok := r.pending[*m.ID]
			if ok {
				delete(r.pending, *m.ID)
			}
			r.mu.Unlock()
			if !ok {
				r.fail(errors.New("unknown or duplicate app-server response id"))
				return
			}
			if m.Error != nil {
				ch <- rpcResult{err: fmt.Errorf("app-server request failed (%d)", m.Error.Code)}
			} else {
				ch <- rpcResult{raw: m.Result}
			}
			continue
		}
		if m.Method == "" {
			r.fail(errors.New("malformed app-server JSON-RPC message"))
			return
		}
		if err := r.onNotify(m.Method, m.Params); err != nil {
			r.fail(err)
			return
		}
	}
	if err := s.Err(); err != nil {
		r.fail(fmt.Errorf("app-server read failed: %w", err))
	} else {
		r.fail(io.EOF)
	}
}

func (r *rpcClient) fail(err error) {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.fatal = err
		pending := r.pending
		r.pending = make(map[uint64]chan rpcResult)
		r.mu.Unlock()
		for _, ch := range pending {
			ch <- rpcResult{err: err}
		}
		close(r.done)
	})
}
func (r *rpcClient) close() error { err := r.rw.Close(); r.fail(io.EOF); return err }
