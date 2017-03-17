// ReverseCloneProxy
// - A reverse proxy with a forking of traffic to a clone
//
// You can proxy traffic to production & staging simultaneously.
// This can be used for development/testing/benchmarking, it can
// also be used to replicate traffic while moving across clouds.
//
// TODO:
// -[Done] Create cli with simple reverse proxy (no clone)
// -[Done] <<Testing/Checkpoint>>
// -[Done] Add struct/interface model for ReverseCloneProxy
// -[Done] Should use ServeHTTP which copies the req and calls ServeTargetHTTP
// -[Done] <<Testing/Checkpoint>>
// -[Done] Add sequential calling of ServeCloneHTTP
// -[Done] <<Testing/Checkpoint>>
// -[Done] Add support for timeouts on a & b side
// -[Done] Sync calling of ServeTargetHTTP & only on success call ServeCloneHTTP
// -[Done] <<Testing/Checkpoint>>
// -[Done] Cleanup Debugging & Add logging similar to what was done for our custom teeproxy
// -[Done] <<Testing/Checkpoint>>
// -[Done] Add in support for percentage of traffic to clone
// -[Done] <<Testing/Checkpoint>>
// -[Done] Add separate context for clone to prevent context cancel exits.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/satori/go.uuid"
	"io"
	"net"
	"net/http"
	//	"net/http/httputil"
	"math/rand"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Console flags
var (
	version_str = "20170317.1 (cavanaug)"
	version     = flag.Bool("v", false, "show version number")
	debug       = flag.Int("debug", 0, "debug log level 0=Error, 1=Warning, 2=Info, 3=Debug, 5=VerboseDebug")
	jsonLogging = flag.Bool("j", false, "write the logs in json for easier processing")

	listen_port = flag.String("l", ":8888", "port to accept requests")
	tls_key     = flag.String("key.pem", "", "path to the TLS private key file")
	tls_cert    = flag.String("cert.pem", "", "path to the TLS certificate file")

	target_url     = flag.String("a", "http://localhost:8080", "where target (A-Side) traffic goes")
	target_timeout = flag.Int("a.timeout", 3, "timeout in seconds for target (A-Side) traffic")
	target_rewrite = flag.Bool("a.rewrite", false, "rewrite the host header when proxying target (A-Side) traffic")

	clone_url     = flag.String("b", "http://localhost:8081", "where clone (B-Side) traffic goes")
	clone_timeout = flag.Int("b.timeout", 3, "timeout in seconds for clone (B-Side) traffic")
	clone_rewrite = flag.Bool("b.rewrite", false, "rewrite the host header when proxying clone (B-Side) traffic")
	clone_percent = flag.Float64("b.percent", 100.0, "float64 percentage of traffic to send to clone (B Side)")
)

// **********************************************************************************
// Begin:  Package components  (TODO: Should probably packagize this...)
// **********************************************************************************

// Heavily derived from
// HTTP reverse proxy handler

// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// onExitFlushLoop is a callback set by tests to detect the state of the
// flushLoop() goroutine.
var onExitFlushLoop func()

// ReverseClonedProxy is an HTTP Handler that takes an incoming request and
// sends it to another server, proxying the response back to the
// client.
type ReverseClonedProxy struct {
	// Director must be a function which modifies
	// the request into a new request to be sent
	// using Transport. Its response is then copied
	// back to the original client unmodified.
	// Director must not access the provided Request
	// after returning.
	Director      func(*http.Request)
	DirectorClone func(*http.Request)

	// The transport used to perform proxy requests.
	// If nil, http.DefaultTransport is used.
	Transport      http.RoundTripper
	TransportClone http.RoundTripper

	// FlushInterval specifies the flush interval
	// to flush to the client while copying the
	// response body.
	// If zero, no periodic flushing is done.
	FlushInterval time.Duration

	// ErrorLog specifies an optional logger for errors
	// that occur when attempting to proxy the request.
	// If nil, logging goes to os.Stderr via the log package's
	// standard logger.

	ErrorLog *log.Logger

	// BufferPool optionally specifies a buffer pool to
	// get byte slices for use by io.CopyBuffer when
	// copying HTTP response bodies.
	BufferPool BufferPool

	// ModifyResponse is an optional function that
	// modifies the Response from the backend.
	// If it returns an error, the proxy returns a StatusBadGateway error.
	ModifyResponse func(*http.Response) error
	//ModifyResponseClone func(*http.Response) error
}

// A BufferPool is an interface for getting and returning temporary
// byte slices for use by io.CopyBuffer.
type BufferPool interface {
	Get() []byte
	Put([]byte)
}

func singleJoiningSlash(a, b string) string {
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

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection", // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",      // canonicalized version of "TE"
	"Trailer", // not Trailers per URL above; http://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding",
	"Upgrade",
}

//
// Serve the http for the Target
// - This is unmodified from ReverseProxy.ServeHTTP except for logging
func (p *ReverseClonedProxy) ServeTargetHTTP(rw http.ResponseWriter, req *http.Request, uid uuid.UUID) int {
	transport := p.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	ctx := req.Context()
	if cn, ok := rw.(http.CloseNotifier); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		notifyChan := cn.CloseNotify()
		go func() {
			select {
			case <-notifyChan:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	outreq := new(http.Request)
	*outreq = *req // includes shallow copies of maps, but okay
	if req.ContentLength == 0 {
		outreq.Body = nil // Issue 16036: nil Body for http.Transport retries
	}
	outreq = outreq.WithContext(ctx)

	p.Director(outreq)
	outreq.Close = false

	// We are modifying the same underlying map from req (shallow
	// copied above) so we only copy it if necessary.
	copiedHeaders := false

	// Remove hop-by-hop headers listed in the "Connection" header.
	// See RFC 2616, section 14.10.
	if c := outreq.Header.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				if !copiedHeaders {
					outreq.Header = make(http.Header)
					copyHeader(outreq.Header, req.Header)
					copiedHeaders = true
				}
				outreq.Header.Del(f)
			}
		}
	}

	// Remove hop-by-hop headers to the backend. Especially
	// important is "Connection" because we want a persistent
	// connection, regardless of what the client sent to us.
	for _, h := range hopHeaders {
		if outreq.Header.Get(h) != "" {
			if !copiedHeaders {
				outreq.Header = make(http.Header)
				copyHeader(outreq.Header, req.Header)
				copiedHeaders = true
			}
			outreq.Header.Del(h)
		}
	}

	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := outreq.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		outreq.Header.Set("X-Forwarded-For", clientIP)
	}

	log.WithFields(log.Fields{
		"uuid":                  uid,
		"side":                  "A-Side",
		"request_method":        outreq.Method,
		"request_path":          outreq.URL.RequestURI(),
		"request_proto":         outreq.Proto,
		"request_host":          outreq.Host,
		"request_contentlength": outreq.ContentLength,
		//"request_context":       outreq.Context(),
	}).Info("Proxy Request")
	res, err := transport.RoundTrip(outreq)
	if err != nil {
		log.WithFields(log.Fields{
			"uuid":          uid,
			"side":          "A-Side",
			"response_code": http.StatusBadGateway,
			"error":         err,
		}).Error("Proxy Response")
		rw.WriteHeader(http.StatusBadGateway)
		return http.StatusBadGateway
	}
	if *debug > 4 {
		log.WithFields(log.Fields{
			"uuid":                   uid,
			"side":                   "A-Side",
			"response_code":          res.StatusCode,
			"response_contentlength": res.ContentLength,
			"response_header":        res.Header,
		}).Debug("Proxy Response (Debug)")
	} else {
		log.WithFields(log.Fields{
			"uuid":                   uid,
			"side":                   "A-Side",
			"response_code":          res.StatusCode,
			"response_contentlength": res.ContentLength,
		}).Info("Proxy Response")
	}

	// Remove hop-by-hop headers listed in the
	// "Connection" header of the response.
	if c := res.Header.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				res.Header.Del(f)
			}
		}
	}

	for _, h := range hopHeaders {
		res.Header.Del(h)
	}

	copyHeader(rw.Header(), res.Header)

	// The "Trailer" header isn't included in the Transport's response,
	// at least for *http.Transport. Build it up from Trailer.
	if len(res.Trailer) > 0 {
		trailerKeys := make([]string, 0, len(res.Trailer))
		for k := range res.Trailer {
			trailerKeys = append(trailerKeys, k)
		}
		rw.Header().Add("Trailer", strings.Join(trailerKeys, ", "))
	}

	rw.WriteHeader(res.StatusCode)
	if len(res.Trailer) > 0 {
		// Force chunking if we saw a response trailer.
		// This prevents net/http from calculating the length for short
		// bodies and adding a Content-Length.
		if fl, ok := rw.(http.Flusher); ok {
			fl.Flush()
		}
	}
	p.copyResponse(rw, res.Body)
	res.Body.Close() // close now, instead of defer, to populate res.Trailer
	copyHeader(rw.Header(), res.Trailer)
	return res.StatusCode
}

//
// Serve the http for the Clone
// - Handles special casing for the clone (ie. No response back to client)
func (p *ReverseClonedProxy) ServeCloneHTTP(req *http.Request, uid uuid.UUID) int {

	transport := p.TransportClone
	if transport == nil {
		transport = http.DefaultTransport
	}

	outreq := new(http.Request)
	*outreq = *req // includes shallow copies of maps, but okay
	if req.ContentLength == 0 {
		outreq.Body = nil // Issue 16036: nil Body for http.Transport retries
	}

	// Hmm.   Im not an expert on how contexts & cancels are handled.
	// Im making potentially a dangerous assumption that giving the clone
	// side a new context, this wont get cancelled on a client.Done.  In essence
	// no cancellation on clone if the target & client are complete.
	outreq = outreq.WithContext(context.TODO())

	p.DirectorClone(outreq)
	outreq.Close = false

	// We are modifying the same underlying map from req (shallow
	// copied above) so we only copy it if necessary.
	copiedHeaders := false

	// Remove hop-by-hop headers listed in the "Connection" header.
	// See RFC 2616, section 14.10.
	if c := outreq.Header.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				if !copiedHeaders {
					outreq.Header = make(http.Header)
					copyHeader(outreq.Header, req.Header)
					copiedHeaders = true
				}
				outreq.Header.Del(f)
			}
		}
	}

	// Remove hop-by-hop headers to the backend. Especially
	// important is "Connection" because we want a persistent
	// connection, regardless of what the client sent to us.
	for _, h := range hopHeaders {
		if outreq.Header.Get(h) != "" {
			if !copiedHeaders {
				outreq.Header = make(http.Header)
				copyHeader(outreq.Header, req.Header)
				copiedHeaders = true
			}
			outreq.Header.Del(h)
		}
	}

	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := outreq.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		outreq.Header.Set("X-Forwarded-For", clientIP)
	}

	log.WithFields(log.Fields{
		"uuid":                  uid,
		"side":                  "B-Side",
		"request_method":        outreq.Method,
		"request_path":          outreq.URL.RequestURI(),
		"request_proto":         outreq.Proto,
		"request_host":          outreq.Host,
		"request_contentlength": outreq.ContentLength,
		//"request_context":       outreq.Context(),
	}).Info("Proxy Request")

	res, err := transport.RoundTrip(outreq)
	if err != nil {
		log.WithFields(log.Fields{
			"uuid":          uid,
			"side":          "B-Side",
			"response_code": http.StatusBadGateway,
			"error":         err,
		}).Error("Proxy Response")
		return http.StatusBadGateway
	}
	if *debug > 4 {
		log.WithFields(log.Fields{
			"uuid":                   uid,
			"side":                   "B-Side",
			"response_code":          res.StatusCode,
			"response_contentlength": res.ContentLength,
			"response_header":        res.Header,
		}).Debug("Proxy Response (Debug)")
	} else {
		log.WithFields(log.Fields{
			"uuid":                   uid,
			"side":                   "B-Side",
			"response_code":          res.StatusCode,
			"response_contentlength": res.ContentLength,
		}).Info("Proxy Response")
	}

	// Remove hop-by-hop headers listed in the
	// "Connection" header of the response.
	if c := res.Header.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				res.Header.Del(f)
			}
		}
	}

	for _, h := range hopHeaders {
		res.Header.Del(h)
	}

	res.Body.Close() // close now, instead of defer, to populate res.Trailer
	return res.StatusCode
}

type nopCloser struct {
	io.Reader
}

func (nopCloser) Close() error { return nil }

// ***************************************************************************
// Handle umbrella ServeHTTP interface
// - Replicates the request
// - Call each of ServeTargetHTTP & ServeCloneHTTP asynchronously
// - Nothing else...
func (p *ReverseClonedProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {

	uid := uuid.NewV4()

	b1 := new(bytes.Buffer)
	b2 := new(bytes.Buffer)
	w := io.MultiWriter(b1, b2)
	io.Copy(w, req.Body)

	target_req := new(http.Request)
	*target_req = *req
	target_req.Body = nopCloser{b1}

	clone_statuscode := 0
	clone_random := rand.New(rand.NewSource(time.Now().UnixNano())).Float64() * 100
	clone_req := new(http.Request)
	*clone_req = *req
	clone_req.Body = nopCloser{b2}

	defer req.Body.Close()

	// Process Target
	target_statuscode := p.ServeTargetHTTP(rw, target_req, uid)

	// Process Clone
	//    iff Target returned without server error
	//        && random number is less than percent
	switch {
	case target_statuscode < 500: // NON-SERVER ERROR
		if *clone_percent == 100.0 || clone_random < *clone_percent {
			clone_statuscode = p.ServeCloneHTTP(clone_req, uid)
		}
	case target_statuscode >= 500: // SERVER ERROR
		log.WithFields(log.Fields{
			"uuid": uid,
			"side": "B-Side",
		}).Info("Proxy Request Skipped")
		return
	}

	// Clone SERVER ERROR after procesed Target
	if clone_statuscode >= 500 {
		log.WithFields(log.Fields{
			"uuid":          uid,
			"side":          "B-Side",
			"response_code": clone_statuscode,
		}).Error("Proxy Response Unfulfilled")
		return
	}

	return
}

func (p *ReverseClonedProxy) copyResponse(dst io.Writer, src io.Reader) {
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

	var buf []byte
	if p.BufferPool != nil {
		buf = p.BufferPool.Get()
	}
	p.copyBuffer(dst, src, buf)
	if p.BufferPool != nil {
		p.BufferPool.Put(buf)
	}
}

func (p *ReverseClonedProxy) copyBuffer(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	if len(buf) == 0 {
		buf = make([]byte, 32*1024)
	}
	var written int64
	for {
		nr, rerr := src.Read(buf)
		if rerr != nil && rerr != io.EOF {
			log.Error("util: CloneProxy read error during resp body copy: %v", rerr)
		}
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if werr != nil {
				return written, werr
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

type writeFlusher interface {
	io.Writer
	http.Flusher
}

type maxLatencyWriter struct {
	dst     writeFlusher
	latency time.Duration

	mu   sync.Mutex // protects Write + Flush
	done chan bool
}

func (m *maxLatencyWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
			m.mu.Lock()
			m.dst.Flush()
			m.mu.Unlock()
		}
	}
}

func (m *maxLatencyWriter) stop() { m.done <- true }

func parseUrlWithDefaults(ustr string) *url.URL {
	u, err := url.ParseRequestURI(ustr)
	if err != nil {
		fmt.Printf("Error: Unable to parse url %s  (Ex.  http://localhost:9001)", ustr)
		os.Exit(1)
	}
	if u.Port() == "" && u.Scheme == "https" {
		u.Host = fmt.Sprintf("%s:443", u.Host)
	}
	if u.Port() == "" && u.Scheme == "http" {
		u.Host = fmt.Sprintf("%s:80", u.Host)
	}
	return u
}

// select a host from the passed `targets`
func NewCloneProxy(target *url.URL, target_timeout int, target_rewrite bool, clone *url.URL, clone_timeout int, clone_rewrite bool) *ReverseClonedProxy {
	targetQuery := target.RawQuery
	cloneQuery := clone.RawQuery
	director := func(req *http.Request) {
		//log.Debug("CALLING DIRECTOR")
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
		if target.Scheme == "https" || target_rewrite {
			req.Host = target.Host
		}
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
		if _, ok := req.Header["User-Agent"]; !ok {
			// explicitly disable User-Agent so it's not set to default value
			req.Header.Set("User-Agent", "")
		}
	}
	directorclone := func(req *http.Request) {
		//log.Debug("CALLING DIRECTOR CLONE")
		req.URL.Scheme = clone.Scheme
		req.URL.Host = clone.Host
		req.URL.Path = singleJoiningSlash(clone.Path, req.URL.Path)
		if clone.Scheme == "https" || clone_rewrite {
			req.Host = clone.Host
		}
		if cloneQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = cloneQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = cloneQuery + "&" + req.URL.RawQuery
		}
		if _, ok := req.Header["User-Agent"]; !ok {
			// explicitly disable User-Agent so it's not set to default value
			req.Header.Set("User-Agent", "")
		}
	}
	return &ReverseClonedProxy{
		Director:      director,
		DirectorClone: directorclone,
		Transport: &http.Transport{
			Dial: (&net.Dialer{
				Timeout:   time.Duration(time.Duration(target_timeout) * time.Second),
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout: 3 * time.Second,
			TLSClientConfig:     &tls.Config{
			//InsecureSkipVerify: true
			},
		},
		TransportClone: &http.Transport{
			Dial: (&net.Dialer{
				Timeout:   time.Duration(time.Duration(clone_timeout) * time.Second),
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout: 3 * time.Second,
			TLSClientConfig:     &tls.Config{
			//InsecureSkipVerify: true
			},
		},
	}
}

func main() {
	flag.Parse()

	if *version {
		fmt.Printf("cloneproxy version: %s\n", version_str)
		os.Exit(0)
	}

	log.SetOutput(os.Stdout)
	// Log as JSON instead of the default ASCII formatter
	if *jsonLogging {
		log.SetFormatter(&log.JSONFormatter{})
	}
	// Set appropriate logging level
	switch {
	case *debug == 0:
		log.SetLevel(log.ErrorLevel)
	case *debug == 1:
		log.SetLevel(log.WarnLevel)
	case *debug == 2:
		log.SetLevel(log.InfoLevel)
	case *debug >= 3:
		log.SetLevel(log.DebugLevel)
	}

	runtime.GOMAXPROCS(runtime.NumCPU() * 2)

	if !strings.HasPrefix(*target_url, "http") {
		fmt.Printf("Error: target url %s is invalid\n   URL's must have a scheme defined, either http or https\n\n", *target_url)
		flag.Usage()
		os.Exit(1)
	}
	if !strings.HasPrefix(*clone_url, "http") {
		fmt.Printf("Error: clone url %s is invalid\n   URL's must have a scheme defined, either http or https\n\n", *clone_url)
		flag.Usage()
		os.Exit(1)
	}

	targetURL := parseUrlWithDefaults(*target_url)
	cloneURL := parseUrlWithDefaults(*clone_url)
	proxy := NewCloneProxy(targetURL, *target_timeout, *target_rewrite, cloneURL, *clone_timeout, *clone_rewrite)

	log.WithFields(log.Fields{
		"version":    version_str,
		"proxy_port": *listen_port,
		"proxy_tls":  len(*tls_key) > 0,
		"a_url":      *target_url,
		"a_timeout":  *target_timeout,
		"a_rewrite":  *target_rewrite,
		"b_url":      *clone_url,
		"b_timeout":  *clone_timeout,
		"b_rewrite":  *clone_rewrite,
		"b_percent":  *clone_percent,
	}).Info("Cloneproxy Initializing")

	if len(*tls_key) > 0 {
		log.Fatal(http.ListenAndServeTLS(*listen_port, *tls_cert, *tls_key, proxy))
	} else {
		log.Fatal(http.ListenAndServe(*listen_port, proxy))
	}
}
