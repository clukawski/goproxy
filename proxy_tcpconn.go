package goproxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"time"

	"github.com/function61/gokit/logex"
)

type proxyTCPConn struct {
	net.Conn
	BytesWrote   int64
	BytesRead    int64
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	Logger       *logex.Leveled
}

// newProxyTCPConn is a wrapper around a net.TCPConn that allows us to log the number of bytes
// written to the connection
func newProxyTCPConn(conn net.Conn) *proxyTCPConn {
	return &proxyTCPConn{Conn: conn}
}

func (conn *proxyTCPConn) Write(b []byte) (n int, err error) {
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

func (conn *proxyTCPConn) Read(b []byte) (n int, err error) {
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

func (conn *proxyTCPConn) setKeepaliveParameters(count, interval, period int) error {
	tcpConn, ok := conn.Conn.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("Could not convert proxy conn from %v to net.TCPConn", reflect.TypeOf(conn.Conn))
	}
	setErr := tcpConn.SetKeepAlive(true)
	if setErr != nil {
		return setErr
	}
	setErr = tcpConn.SetKeepAlivePeriod(time.Duration(period) * time.Second)
	if setErr != nil {
		return setErr
	}
	// rawConn, err := tcpConn.SyscallConn()
	// if err != nil {
	// 	return err
	// }
	// err = rawConn.Control(
	// 	func(fdPtr uintptr) {
	// 		// got socket file descriptor. Setting parameters.
	// 		fd := int(fdPtr)

	// 		conn.Logger.Info.Printf("attempting KA options on: %+v - fd: %+v", rawConn, fd)

	// 		//Number of probes.
	// 		err := syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, count)
	// 		if err != nil {
	// 			conn.Logger.Error.Printf("on setting keepalive probe count: %s", err.Error())
	// 		}
	// 		//Wait time after an unsuccessful probe.
	// 		err = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, interval)
	// 		if err != nil {
	// 			conn.Logger.Error.Printf("on setting keepalive retry interval: %s", err.Error())
	// 		}
	// 	})
	// if err != nil {
	// 	return err
	// }
	return nil
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
