//go:build linux

// Linux connection I/O: up to maxBlockingConns connections are served with plain blocking read/write
// system calls on their own socket (one read and one write per request, no epoll round trip and no
// thread hand-off); further connections use Go's poller as usual.
package main

import (
	"io"
	"net"
	"sync/atomic"
	"syscall"
)

const maxBlockingConns = 16

var blockingConns atomic.Int32

type fdIO struct{ fd int }

func (f fdIO) Read(p []byte) (int, error) {
	for {
		n, err := syscall.Read(f.fd, p)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

func (f fdIO) Write(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := syscall.Write(f.fd, p[total:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func connIO(conn net.Conn) (io.Reader, io.Writer, func()) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return conn, conn, func() {}
	}
	if blockingConns.Add(1) > maxBlockingConns {
		blockingConns.Add(-1)
		return conn, conn, func() {}
	}
	release := func() { blockingConns.Add(-1) }
	rc, err := tc.SyscallConn()
	if err != nil {
		release()
		return conn, conn, func() {}
	}
	fd := -1
	if err := rc.Control(func(s uintptr) { fd = int(s) }); err != nil || fd < 0 {
		release()
		return conn, conn, func() {}
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		release()
		return conn, conn, func() {}
	}
	return fdIO{fd}, fdIO{fd}, release
}
