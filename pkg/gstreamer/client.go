//go:build linux

package gstreamer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/magic"
)

// Request is the JSON payload sent to the external gstreamer service. Share
// pipelines run as their own gst-launch processes; Result runs as one process
// whose fdsink is bound to the FD passed alongside this request. The service
// replies with {"status":"ok"} on success.
type Request struct {
	Action string            `json:"action"`
	Result string            `json:"result"`
	Share  map[string]string `json:"share,omitempty"`
}

type response struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

const (
	handshakeDeadline = 5 * time.Second
	defaultReadBuf    = 1024
)

// NewProducer dials the gstreamer service at socket, sends req with the
// write end of a local pipe (SCM_RIGHTS), and returns a Producer that
// reads the stream through pkg/magic.
func NewProducer(socket string, req Request) (*Producer, error) {
	if socket == "" {
		return nil, errors.New("gstreamer: empty socket address")
	}
	if req.Action == "" {
		req.Action = "start"
	}
	if req.Result == "" {
		return nil, errors.New("gstreamer: empty result pipeline")
	}

	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	conn, err := dial(socket, &req, w)
	_ = w.Close() // FD now owned by the service via SCM_RIGHTS
	if err != nil {
		_ = r.Close()
		return nil, err
	}

	prod, err := magic.Open(r)
	if err != nil {
		_ = conn.Close()
		_ = r.Close()
		return nil, fmt.Errorf("gstreamer: %w", err)
	}

	return &Producer{wrapped: prod, conn: conn, readPipe: r}, nil
}

func dial(socket string, req *Request, fd *os.File) (*net.UnixConn, error) {
	addr := &net.UnixAddr{Name: socket, Net: "unix"}
	conn, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	oob := syscall.UnixRights(int(fd.Fd()))
	if _, _, err = conn.WriteMsgUnix(data, oob, nil); err != nil {
		_ = conn.Close()
		return nil, err
	}

	if err := conn.SetReadDeadline(time.Now().Add(handshakeDeadline)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("gstreamer: set deadline: %w", err)
	}

	var buf [defaultReadBuf]byte
	n, _, _, _, err := conn.ReadMsgUnix(buf[:], nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("gstreamer: handshake: %w", err)
	}

	var resp response
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("gstreamer: handshake decode: %w", err)
	}
	if resp.Status != "ok" {
		_ = conn.Close()
		return nil, errors.New("gstreamer: " + resp.Error)
	}

	return conn, nil
}
