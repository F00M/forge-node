//go:build !linux

// Other platforms (local development builds) use Go's poller for every connection.
package main

import (
	"io"
	"net"
)

func connIO(conn net.Conn) (io.Reader, io.Writer, func()) { return conn, conn, func() {} }
