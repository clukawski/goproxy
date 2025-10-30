package goproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ProxyLeveledLogger is used to get leveled syslog logging
type ProxyLeveledLogger struct {
	Warningf func(format string, a ...interface{}) error
	Debugf   func(format string, a ...interface{}) error
	Infof    func(format string, a ...interface{}) error
}

// ProxyCtx is the Proxy context, contains useful information about every request. It is passed to
// every user function. Also used as a logger.
type ProxyCtx struct {
	// Will contain the client request from the proxy
	Req *http.Request
	// Will contain the remote server's response (if available. nil if the request wasn't send yet)
	Resp         *http.Response
	RoundTripper RoundTripper
	// will contain the recent error that occurred while trying to send receive or parse traffic
	Error error
	// A handle for the user to keep data in the context, from the call of ReqHandler to the
	// call of RespHandler
	UserData interface{}
	// Will connect a request to a response
	Session   int64
	certStore CertStorage
	Proxy     *ProxyHttpServer

	// Cancel can be used to ensure contexts created during the request
	// in callbacks provided to DoFunc and HandleConnectFunc are cancelled
	// when the request is complete.
	//
	// Behaviour is unchanged if Cancel is nil.
	Cancel context.CancelFunc

	ProxyLogger                          *ProxyLeveledLogger
	LogRequestID                         string
	EDNSClientSubnetV4                   string
	EDNSClientSubnetV6                   string
	ForwardProxy                         string
	ForwardProxyDialTimeout              int
	ForwardProxyTLSTimeout               int
	ForwardProxyAuth                     string
	ForwardProxyProto                    string
	ForwardProxyHeaders                  []ForwardProxyHeader
	ForwardProxyStripHeaders             []string
	ForwardMetricsCounters               MetricsCounters
	ForwardProxyRegWrite                 bool
	ForwardProxyErrorFallbackAuth        bool
	ForwardProxyErrorFallback            func() (string, string)
	ForwardProxyFallbackTimeout          int
	ForwardProxyFallbackSecondaryTimeout int
	ForwardProxyTProxy                   bool
	ForwardProxyLocalRequest             bool
	ForwatdTProxyDropIP                  string
	ForwardProxySourceIP                 string
	ForwardProxySourceIPv6               string
	ForwardProxyIPv6OnlyExitLocal        bool
	ForwardProxyDirect                   bool
	ForwardProxyDirectSendOK             bool
	ForwardDisableHTTPKeepAlives         bool
	CloseOnError                         bool
	DNSResolver                          string
	BackupDNSResolver                    string
	DNSLocalAddr                         string
	DNSTimeout                           time.Duration
	TCPKeepAlivePeriod                   int
	TCPKeepAliveCount                    int
	TCPKeepAliveInterval                 int
	ProxyTargetAddress                   string
	ProxyUser                            string
	MaxIdleConns                         int
	MaxIdleConnsPerHost                  int
	MaxConnsPerHost                      int
	IdleConnTimeout                      time.Duration
	ProxyReadDeadline                    int
	ProxyWriteDeadline                   int
	CopyBufferSize                       int
	Accounting                           string
	BytesSent                            int64
	BytesReceived                        int64
	Tail                                 func(*ProxyCtx) error
}

type MetricsCounters struct {
	Requests       *prometheus.CounterVec
	ProxyBandwidth *prometheus.Counter
	TLSTimes       *prometheus.Observer
}

type ForwardProxyHeader struct {
	Header string
	Value  string
}

type RoundTripper interface {
	RoundTrip(req *http.Request, ctx *ProxyCtx) (*http.Response, error)
}

type CertStorage interface {
	Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error)
}

type RoundTripperFunc func(req *http.Request, ctx *ProxyCtx) (*http.Response, error)

func (f RoundTripperFunc) RoundTrip(req *http.Request, ctx *ProxyCtx) (*http.Response, error) {
	return f(req, ctx)
}

func (ctx *ProxyCtx) SetErrorMetric() {
	if ctx.ForwardProxy != "" && ctx.ForwardMetricsCounters.Requests != nil {

		var target string
		if strings.HasPrefix(ctx.ForwardProxy, "127.0.0.1") {
			target = "local"
		} else {
			target = "spoof"
		}
		ctx.ForwardMetricsCounters.Requests.WithLabelValues(target, "err").Inc()

	}
}

func (ctx *ProxyCtx) SetSuccessMetric() {
	if ctx.ForwardProxy != "" && ctx.ForwardMetricsCounters.Requests != nil {

		var target string
		if strings.HasPrefix(ctx.ForwardProxy, "127.0.0.1") {
			target = "local"
		} else {
			target = "spoof"
		}
		ctx.ForwardMetricsCounters.Requests.WithLabelValues(target, "ok").Inc()

	}
}

func (ctx *ProxyCtx) RoundTrip(req *http.Request) (*http.Response, error) {
	if ctx.RoundTripper != nil {
		return ctx.RoundTripper.RoundTrip(req, ctx)
	}
	var tr *http.Transport

	dialTimeout := ctx.ForwardProxyDialTimeout
	if dialTimeout == 0 {
		dialTimeout = 20
	}
	d := net.Dialer{
		Timeout:  time.Duration(dialTimeout) * time.Second,
		Resolver: ctx.Proxy.getResolver(ctx, "udp", ""),
	}

	if ctx.ForwardProxySourceIP != "" {
		localAddr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(ctx.ForwardProxySourceIP, "0"))
		if err != nil {
			ctx.Logf("Failed to resolve local address: %s - err: %v", ctx.ForwardProxySourceIP, err)
		} else {
			d.LocalAddr = localAddr
		}
	}

	host := req.URL.Host
	if !strings.Contains(req.URL.Host, ":") {
		host = req.URL.Host + ":80"
	}

	//check for idle override
	var idleTimeout time.Duration
	if ctx.IdleConnTimeout != 0 {
		idleTimeout = ctx.IdleConnTimeout
	} else {
		idleTimeout = 90 * time.Second
	}

	//max conns
	var maxConns int
	if ctx.MaxIdleConns != 0 {
		maxConns = ctx.MaxIdleConns
	} else {
		maxConns = 100
	}

	//max per host
	var maxPerHostConns int
	if ctx.MaxIdleConnsPerHost != 0 {
		maxPerHostConns = ctx.MaxIdleConnsPerHost
	} else {
		maxPerHostConns = 2
	}

	// Create our transport depending on behaviour (normal/proxied)
	var rawConn net.Conn
	var err error

	setTargetKA := false

	if ctx.ForwardProxy != "" {

		if ctx.ForwardProxyProto == "" {
			ctx.ForwardProxyProto = "http"
		}

		tlsTimeout := ctx.ForwardProxyTLSTimeout
		if tlsTimeout == 0 {
			tlsTimeout = 15
		}

		tr = &http.Transport{
			MaxIdleConns:          maxConns,
			MaxIdleConnsPerHost:   maxPerHostConns,
			TLSHandshakeTimeout:   time.Duration(tlsTimeout) * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       idleTimeout,
			Proxy: func(req *http.Request) (*url.URL, error) {
				return url.Parse(ctx.ForwardProxyProto + "://" + ctx.ForwardProxy)
			},
			Dial: ctx.Proxy.NewConnectDialWithKeepAlives(ctx, ctx.ForwardProxyProto+"://"+ctx.ForwardProxy, func(req *http.Request) {
				if ctx.ForwardProxyAuth != "" {
					req.Header.Set("Proxy-Authorization", fmt.Sprintf("Basic %s", ctx.ForwardProxyAuth))
				}
				if len(ctx.ForwardProxyHeaders) > 0 {
					for _, pxyHeader := range ctx.ForwardProxyHeaders {
						ctx.Logf("setting proxy header %+v", pxyHeader)
						// req.Header.Set(pxyHeader.Header, pxyHeader.Value)
						// Manually set the header so that we avoid canonicalization
						req.Header[pxyHeader.Header] = []string{pxyHeader.Value}
					}
				}
			}),
		}

		if ctx.ForwardProxyFallbackTimeout > 0 {
			tr.DialContext = (&net.Dialer{
				Timeout:   time.Duration(int64(ctx.ForwardProxyFallbackTimeout)) * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
				Resolver:  ctx.Proxy.getResolver(ctx, "udp", ""),
			}).DialContext
			if ctx.ForwardProxyFallbackSecondaryTimeout > 0 {
				ctx.ForwardProxyFallbackTimeout = ctx.ForwardProxyFallbackSecondaryTimeout
			} else {
				ctx.ForwardProxyFallbackTimeout = 10
			}
		}

		dialStart := time.Now().UnixNano()

		rawConn, err = tr.Dial("tcp4", host)

		dialEnd := time.Now().UnixNano()

		if err != nil {
			c4, c6, err := ctx.Proxy.resolveDomain(ctx, "udp", strings.Split(host, ":")[0], ctx.DNSResolver)
			if err != nil && ctx.BackupDNSResolver != "" {
				c4, c6, err = ctx.Proxy.resolveDomain(ctx, "udp", strings.Split(host, ":")[0], ctx.BackupDNSResolver)
			}
			if len(c4) > 0 && len(c6) > 0 {
				ctx.Logf("error-metric: http dial to %s failed: %v", host, err)
				ctx.SetErrorMetric()
			}
			// if a fallback func was provided, retry
			if ctx.ForwardProxyErrorFallback != nil {
				newForwardProxy, extra := ctx.ForwardProxyErrorFallback()
				ctx.ForwardProxyErrorFallback = nil
				if newForwardProxy != "" {
					ctx.ForwardProxy = newForwardProxy
					if ctx.ForwardProxyErrorFallbackAuth {
						ctx.ForwardProxyAuth = extra
					} else {
						ctx.Accounting = extra
					}
					return ctx.RoundTrip(req)
				}
			}
			return nil, err
		}

		if ctx.ForwardMetricsCounters.TLSTimes != nil {
			tlsTime := float64(dialEnd/1000000) - float64(dialStart/1000000)
			metric := *ctx.ForwardMetricsCounters.TLSTimes
			metric.Observe(float64(tlsTime))
		}

	} else {

		setTargetKA = true

		tlsTimeout := ctx.ForwardProxyTLSTimeout
		if tlsTimeout == 0 {
			tlsTimeout = 15
		}
		// Dial with regular transport
		tr = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			Dial:                  d.Dial,
			MaxIdleConns:          maxConns,
			MaxIdleConnsPerHost:   maxPerHostConns,
			IdleConnTimeout:       idleTimeout,
			DisableKeepAlives:     ctx.ForwardDisableHTTPKeepAlives,
			TLSHandshakeTimeout:   time.Duration(tlsTimeout) * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}

		rawConn, err = tr.Dial("tcp4", host)
		if err != nil {
			return nil, err
		}
	}

	req.RequestURI = req.URL.String()

	conn := newProxyTCPConn(rawConn)
	conn.Logger = ctx.ProxyLogger
	conn.ReadTimeout = time.Second * 5
	conn.WriteTimeout = time.Second * 5
	conn.IgnoreDeadlineErrors = true

	//set tcp keep alives.
	tcpKAPeriod := 5
	if ctx.TCPKeepAlivePeriod > 0 {
		tcpKAPeriod = ctx.TCPKeepAlivePeriod
	}
	tcpKACount := 3
	if ctx.TCPKeepAliveCount > 0 {
		tcpKACount = ctx.TCPKeepAliveCount
	}
	tcpKAInterval := 3
	if ctx.TCPKeepAliveInterval > 0 {
		tcpKAInterval = ctx.TCPKeepAliveInterval
	}
	if setTargetKA {
		kaErr := conn.SetKeepaliveParameters(false, tcpKACount, tcpKAInterval, tcpKAPeriod)
		if kaErr != nil {
			ctx.Logf("HTTP conn KeepAlive error: %v", kaErr)
			conn.ReadTimeout = time.Second * time.Duration(ctx.ProxyReadDeadline)
			conn.WriteTimeout = time.Second * time.Duration(ctx.ProxyWriteDeadline)
			conn.IgnoreDeadlineErrors = false
		}
	}

	bufferSize := 32

	if ctx.CopyBufferSize > 0 {
		bufferSize = ctx.CopyBufferSize
	}

	reader := bufio.NewReaderSize(conn, bufferSize*1024)
	writer := bufio.NewWriterSize(conn, bufferSize*1024)
	readDone := make(chan responseAndError, 1)
	writeDone := make(chan error, 1)

	// Write the request.
	go func(pconn *ProxyTCPConn) {
		var err error

		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", "")
		}

		// Use writeproxy so as to not strip RequestURI if we
		// are forwarding to another proxy
		if ctx.ForwardProxy != "" && ctx.ForwardProxyRegWrite == false {
			err = req.WriteProxy(writer)
		} else {
			err = req.Write(writer)
		}

		if err == nil {
			writer.Flush()
		} else {
			ctx.Logf("req.Write failed: %v - conn read %v, conn written %v", err, pconn.BytesRead, pconn.BytesWrote)
		}

		writeDone <- err
	}(conn)

	// And read the response.
	go func() {
		resp, err := http.ReadResponse(reader, req)
		if err != nil {
			readDone <- responseAndError{nil, err}
			return
		}

		resp.Body = &connCloser{resp.Body, conn.Conn}

		readDone <- responseAndError{resp, nil}
	}()

	if err := <-writeDone; err != nil {
		ctx.Logf("error-metric: writeDone failed: %v - conn read %v, conn written %v", err, conn.BytesRead, conn.BytesWrote)
		if !strings.Contains(err.Error(), "timeout") {
			ctx.SetErrorMetric()
		}
		return nil, err
	}

	ctx.BytesSent = conn.BytesWrote
	ctx.BytesReceived = conn.BytesRead

	r := <-readDone
	if r.err != nil {
		ctx.Logf("error-metric: readDone failed: %v", r.err)
		if !strings.Contains(r.err.Error(), "timeout") {
			ctx.SetErrorMetric()
		}
		return nil, r.err
	}

	ctx.SetSuccessMetric()
	if ctx.ForwardMetricsCounters.ProxyBandwidth != nil {
		metric := *ctx.ForwardMetricsCounters.ProxyBandwidth
		metric.Add(float64(conn.BytesWrote + conn.BytesRead))
	}
	return r.resp, nil
}

func (ctx *ProxyCtx) printf(msg string, argv ...interface{}) {
	if ctx.Proxy.Verbose {
		if ctx.LogRequestID != "" {
			ctx.Proxy.Logger.Printf("[%s] "+msg+"\n", append([]interface{}{ctx.LogRequestID}, argv...)...)
		} else {
			ctx.Proxy.Logger.Printf("[%03d] "+msg+"\n", append([]interface{}{ctx.Session & 0xFF}, argv...)...)
		}
	}
}

// Logf prints a message to the proxy's log. Should be used in a ProxyHttpServer's filter
// This message will be printed only if the Verbose field of the ProxyHttpServer is set to true
//
//	proxy.OnRequest().DoFunc(func(r *http.Request,ctx *goproxy.ProxyCtx) (*http.Request, *http.Response){
//		nr := atomic.AddInt32(&counter,1)
//		ctx.Printf("So far %d requests",nr)
//		return r, nil
//	})
func (ctx *ProxyCtx) Logf(msg string, argv ...interface{}) {
	if ctx.ProxyLogger != nil {
		if ctx.LogRequestID != "" {
			ctx.ProxyLogger.Debugf("[%s] "+msg, append([]interface{}{ctx.LogRequestID}, argv...)...)
		} else {
			ctx.ProxyLogger.Debugf("[%03d] "+msg, append([]interface{}{ctx.Session & 0xFF}, argv...)...)
		}
		return
	}
	ctx.printf(msg, argv...)
}

func (ctx *ProxyCtx) Infof(msg string, argv ...interface{}) {
	if ctx.ProxyLogger != nil {
		if ctx.LogRequestID != "" {
			ctx.ProxyLogger.Infof("[%s] "+msg, append([]interface{}{ctx.LogRequestID}, argv...)...)
		} else {
			ctx.ProxyLogger.Infof("[%03d] "+msg, append([]interface{}{ctx.Session & 0xFF}, argv...)...)
		}
		return
	}
	ctx.printf(msg, argv...)
}

// Warnf prints a message to the proxy's log. Should be used in a ProxyHttpServer's filter
// This message will always be printed.
//
//	proxy.OnRequest().DoFunc(func(r *http.Request,ctx *goproxy.ProxyCtx) (*http.Request, *http.Response){
//		f,err := os.OpenFile(cachedContent)
//		if err != nil {
//			ctx.Warnf("error open file %v: %v",cachedContent,err)
//			return r, nil
//		}
//		return r, nil
//	})
func (ctx *ProxyCtx) Warnf(msg string, argv ...interface{}) {
	if ctx.ProxyLogger != nil {
		if ctx.LogRequestID != "" {
			ctx.ProxyLogger.Warningf("[%s] "+msg, append([]interface{}{ctx.LogRequestID}, argv...)...)
		} else {
			ctx.ProxyLogger.Warningf("[%03d] "+msg, append([]interface{}{ctx.Session & 0xFF}, argv...)...)
		}
		return
	}
	ctx.printf(msg, argv...)
}

var charsetFinder = regexp.MustCompile("charset=([^ ;]*)")

// Will try to infer the character set of the request from the headers.
// Returns the empty string if we don't know which character set it used.
// Currently it will look for charset=<charset> in the Content-Type header of the request.
func (ctx *ProxyCtx) Charset() string {
	charsets := charsetFinder.FindStringSubmatch(ctx.Resp.Header.Get("Content-Type"))
	if charsets == nil {
		return ""
	}
	return charsets[1]
}
