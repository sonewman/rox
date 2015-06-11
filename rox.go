package rox

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Rox struct {
	Incoming      func(*Rox, *http.Request)
	MakeRequest   func(*Rox, http.ResponseWriter, *http.Request, *http.Request)
	Target        *url.URL
	Transport     http.RoundTripper
	ErrorLog      *log.Logger
	FlushInterval time.Duration
}

func New(target *url.URL) *Rox {
	return &Rox{
		MakeRequest: DefaultMakeRequest,
		Target:      target,
	}
}

func concatPath(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te", // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

var ignoreHeaders = func() map[string]bool {
	m := make(map[string]bool)
	for _, h := range hopHeaders {
		m[h] = true
	}
	return m
}()

// helper for iterating over http.Header
func IterateHeader(src http.Header, it func(string, string)) {
	for k, vv := range src {
		for _, v := range vv {
			it(k, v)
		}
	}
}

// helper for copying one set of headers to another
func CopyHeader(dst http.Header, src http.Header) {
	IterateHeader(src, func(k string, v string) {
		dst.Add(k, v)
	})
}

type requestCanceler interface {
	CancelRequest(*http.Request)
}

type runOnFirstRead struct {
	io.Reader
	fn func() // Run before first Read, then set to nil
}

func (c *runOnFirstRead) Read(bs []byte) (int, error) {
	if c.fn != nil {
		c.fn()
		c.fn = nil
	}
	return c.Reader.Read(bs)
}

type outgoingResponseBody struct {
	io.Reader
	io.Closer
}

func (p *Rox) streamOutgoingBody(rw http.ResponseWriter, out *http.Request) {
	if closeNotifier, ok := rw.(http.CloseNotifier); ok {
		if requestCanceler, ok := p.Transport.(requestCanceler); ok {
			reqDone := make(chan struct{})
			defer close(reqDone)
			clientGone := closeNotifier.CloseNotify()

			out.Body = &outgoingResponseBody{
				Reader: &runOnFirstRead{
					Reader: out.Body,
					fn: func() {
						go func() {
							select {
							case <-clientGone:
								requestCanceler.CancelRequest(out)
							case <-reqDone:
							}
						}()
					},
				},
				Closer: out.Body,
			}
		}
	}
}

func (p *Rox) newOutgoingRequest(rw http.ResponseWriter, incoming *http.Request) *http.Request {
	if p.Transport == nil {
		p.Transport = http.DefaultTransport
	}

	out := new(http.Request)
	*out = *incoming // includes shallow copies of maps, but okay

	p.streamOutgoingBody(rw, out)
	return out
}

func MakeRequest(p *Rox, out *http.Request) (*http.Response, error) {
	transport := p.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	// start RoundTrip
	res, err := transport.RoundTrip(out)
	return res, err
}

func DefaultMakeRequest(p *Rox, rw http.ResponseWriter, in *http.Request, out *http.Request) {
	// last minute check of scheme
	if out.URL.Scheme == "" {
		out.URL.Scheme = "http"
	}

	res, err := MakeRequest(p, out)

	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
	} else {
		// defer closing response body until after execution
		defer res.Body.Close()
		WriteResponse(p, rw, res)
	}
}

func WriteResponse(p *Rox, rw http.ResponseWriter, res *http.Response) {
	CopyHeader(rw.Header(), res.Header)
	rw.WriteHeader(res.StatusCode)
	p.CopyResponse(rw, res.Body)
}

func (p *Rox) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if p.Incoming != nil {
		p.Incoming(p, req)
	}

	// create outgoing request
	out := p.newOutgoingRequest(rw, req)

	// set this before so it can be
	// overridden if need be
	out.Proto = "HTTP/1.1"
	out.ProtoMajor = 1
	out.ProtoMinor = 1
	out.Close = false

	// Remove hop-by-hop headers to the backend.  Especially
	// important is "Connection" because we want a persistent
	// connection, regardless of what the client sent to us.  This
	// is modifying the same underlying map from req (shallow
	// copied above) so we only copy it if necessary.
	copiedHeaders := false
	for _, h := range hopHeaders {
		if out.Header.Get(h) != "" {
			if !copiedHeaders {
				out.Header = make(http.Header)
				CopyHeader(out.Header, req.Header)
				copiedHeaders = true
			}
			out.Header.Del(h)
		}
	}

	// if we have a target then use it's
	// defined options
	if p.Target != nil {
		p.transferTarget(req, out)

		// otherwise we want to infer the host header
		// provided it is not the same host as us
	} else if out.URL.Host != out.Host {
		out.URL.Host = out.Host
	} else {
		panic(errors.New("Cannot proxy http request to same domain"))
	}

	// call trigger MakeRequest hook
	p.MakeRequest(p, rw, req, out)
}

func (p *Rox) transferTarget(req *http.Request, out *http.Request) {
	// set scheme of request ot that of the target
	out.URL.Scheme = p.Target.Scheme

	// set Host of Request and Header
	out.URL.Host = p.Target.Host
	out.Host = p.Target.Host

	// concat path
	out.URL.Path = concatPath(p.Target.Path, out.URL.Path)

	// concat querystring
	targetQuery := p.Target.RawQuery
	if targetQuery == "" || req.URL.RawQuery == "" {
		req.URL.RawQuery = targetQuery + req.URL.RawQuery
	} else {
		req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
	}
}

func (p *Rox) CopyResponse(dst io.Writer, src io.Reader) {
	if p.FlushInterval != 0 {
		if wf, ok := dst.(writeFlusher); ok {
			mlw := &maxLatencyWriter{
				dst:     wf,
				latency: p.FlushInterval,
				done:    make(chan bool),
			}
			go mlw.flushLoop()
			defer mlw.stop()
			dst = mlw
		}
	}

	io.Copy(dst, src)
}

func (p *Rox) logf(format string, args ...interface{}) {
	if p.ErrorLog != nil {
		p.ErrorLog.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// onExitFlushLoop is a callback set by tests to detect the state of the
// flushLoop() goroutine.
var onExitFlushLoop func()

type writeFlusher interface {
	io.Writer
	http.Flusher
}

type maxLatencyWriter struct {
	dst     writeFlusher
	latency time.Duration

	lk   sync.Mutex // protects Write + Flush
	done chan bool
}

func (m *maxLatencyWriter) Write(p []byte) (int, error) {
	m.lk.Lock()
	defer m.lk.Unlock()
	return m.dst.Write(p)
}

func (m *maxLatencyWriter) flushLoop() {
	t := time.NewTicker(m.latency)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			if onExitFlushLoop != nil {
				onExitFlushLoop()
			}
			return
		case <-t.C:
			m.lk.Lock()
			m.dst.Flush()
			m.lk.Unlock()
		}
	}
}

func (m *maxLatencyWriter) stop() { m.done <- true }
