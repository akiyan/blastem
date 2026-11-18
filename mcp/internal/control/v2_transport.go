package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

type v2Reply struct {
	ID             uint64          `json:"id"`
	Version        int             `json:"version"`
	OK             bool            `json:"ok"`
	Result         json.RawMessage `json:"result"`
	Error          *ProtocolError  `json:"error"`
	transportError error
}

type v2Transport struct {
	conn    net.Conn
	mu      sync.Mutex // serializes ID assignment and writes
	next    uint64
	pending map[uint64]chan v2Reply
	closed  error
}

func newV2Transport(conn net.Conn) *v2Transport {
	v := &v2Transport{conn: conn, pending: make(map[uint64]chan v2Reply)}
	go v.read()
	return v
}

func (v *v2Transport) close(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed != nil {
		return
	}
	v.closed = err
	_ = v.conn.Close()
	for id, ch := range v.pending {
		ch <- v2Reply{transportError: err}
		delete(v.pending, id)
	}
}

func (v *v2Transport) read() {
	reader := bufio.NewReaderSize(v.conn, 65537)
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			v.close(err)
			return
		}
		if len(line) > 65536 {
			v.close(fmt.Errorf("v2 reply exceeds limit"))
			return
		}
		var reply v2Reply
		if err := json.Unmarshal(line, &reply); err != nil {
			v.close(err)
			return
		}
		v.mu.Lock()
		ch := v.pending[reply.ID]
		if reply.Version != 2 || ch == nil {
			v.mu.Unlock()
			v.close(fmt.Errorf("v2 reply identity mismatch"))
			return
		}
		delete(v.pending, reply.ID)
		ch <- reply
		v.mu.Unlock()
	}
}

func (v *v2Transport) request(name string, params map[string]any, out any, timeout time.Duration) error {
	return v.requestReady(name, params, out, timeout, nil)
}

func (v *v2Transport) requestReady(name string, params map[string]any, out any, timeout time.Duration, ready func()) error {
	ch := make(chan v2Reply, 1)
	v.mu.Lock()
	if v.closed != nil {
		err := v.closed
		v.mu.Unlock()
		return err
	}
	v.next++
	id := v.next
	v.pending[id] = ch
	_ = v.conn.SetWriteDeadline(time.Now().Add(timeout))
	err := json.NewEncoder(v.conn).Encode(map[string]any{"id": id, "version": 2, "command": name, "params": params})
	v.mu.Unlock()
	if err != nil {
		v.close(err)
		return err
	}
	if ready != nil {
		ready()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reply := <-ch:
		if reply.transportError != nil {
			return reply.transportError
		}
		if !reply.OK {
			if reply.Error != nil {
				return reply.Error
			}
			return fmt.Errorf("v2 failure without error code")
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(reply.Result, out)
	case <-timer.C:
		err := fmt.Errorf("v2 %s timed out", name)
		v.close(err) // EOF also asks the core to stop an outstanding frame run.
		return err
	}
}
