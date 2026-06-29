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

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/magic"
)

const (
	handshakeDeadline = 5 * time.Second
	defaultReadBuf    = 1024
)

// NewProducer dials the gstreamer service, sends the request with the pipe
// write end via SCM_RIGHTS, and returns a Producer reading the stream through
// pkg/magic. rawURL goes to the wrapped Source field; shareSocket is also
// rendered under "pipelines" in the JSON output.
func NewProducer(rawURL string, shareSocket *ShareSocket) (*Producer, error) {
	if shareSocket == nil || shareSocket.Unixsocket == "" {
		return nil, errors.New("gstreamer: empty socket address")
	}
	if shareSocket.Result == "" {
		return nil, errors.New("gstreamer: empty result pipeline")
	}

	req := Request{
		Action: "start",
		Result: shareSocket.Result,
		Share:  shareSocket.Share,
	}

	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	conn, err := dial(shareSocket.Unixsocket, &req, w)
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

	// magic.Open producers embed core.Connection.
	if info, ok := prod.(core.Info); ok {
		info.SetSource(rawURL)
		info.SetProtocol("unixsocket+pipe")
	}

	return &Producer{
		wrapped:     prod,
		conn:        conn,
		readPipe:    r,
		shareSocket: shareSocket,
	}, nil
}

func dial(socket string, req *Request, fd *os.File) (*net.UnixConn, error) {
	addr := &net.UnixAddr{Name: socket, Net: "unix"}
	conn, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("gstreamer: dial: %w", err)
	}

	data, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("gstreamer: marshal request: %w", err)
	}

	oob := syscall.UnixRights(int(fd.Fd()))
	if _, _, err = conn.WriteMsgUnix(data, oob, nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("gstreamer: send request: %w", err)
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

	var resp Response
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
