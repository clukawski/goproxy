package goproxy

import (
	"io"
	"net"
	"net/http"
	"time"
)

type proxyConn struct {
	net.Conn
	BytesWrote   int64
	BytesRead    int64
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// newProxyConn is a wrapper around a net.Conn that allows us to log the number of bytes
// written to the connection
func newProxyConn(conn net.Conn) *proxyConn {
	c := &proxyConn{Conn: conn}
	return c
}

func (conn *proxyConn) Write(b []byte) (n int, err error) {
	if conn.WriteTimeout > 0 {
		conn.Conn.SetWriteDeadline(time.Now().Add(conn.WriteTimeout))
	}
	n, err = conn.Conn.Write(b)
	if err != nil {
		return
	}
	conn.BytesWrote += int64(n)
	conn.Conn.SetWriteDeadline(time.Time{})
	return
}

func (conn *proxyConn) Read(b []byte) (n int, err error) {
	if conn.ReadTimeout > 0 {
		conn.Conn.SetReadDeadline(time.Now().Add(conn.ReadTimeout))
	}
	n, err = conn.Conn.Read(b)
	if err != nil {
		return
	}
	conn.BytesRead += int64(n)
	conn.Conn.SetReadDeadline(time.Time{})
	return
}

type responseAndError struct {
	resp *http.Response
	err  error
}

// connCloser implements a wrapper containing an io.ReadCloser and a net.Conn
type connCloser struct {
	io.ReadCloser
	Conn net.Conn
}

// Close closes the connection and the io.ReadCloser
func (cc connCloser) Close() error {
	cc.Conn.Close()
	return cc.ReadCloser.Close()
}
