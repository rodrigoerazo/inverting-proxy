/*
Copyright 2017 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package utils defines utilities for the agent.
package utils

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/golang/groupcache/lru"
	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"
)

const (
	// PendingPath is the URL subpath for pending requests held by the proxy.
	PendingPath = "agent/pending"

	// RequestPath is the URL subpath for reading a specific request held by the proxy.
	RequestPath = "agent/request"

	// ResponsePath is the URL subpath for posting a request response to the proxy.
	ResponsePath = "agent/response"

	// HeaderUserID is the name of a response header used by the proxy to identify the end user.
	HeaderUserID = "X-Inverting-Proxy-User-ID"

	// HeaderBackendID is the name of a request header used to uniquely identify this agent.
	HeaderBackendID = "X-Inverting-Proxy-Backend-ID"

	// HeaderVMID is the name of a request header used to report the VM
	// (if any) on which the agent is running.
	HeaderVMID = "X-Inverting-Proxy-VM-ID"

	// HeaderRequestID is the name of a request/response header used to uniquely
	// identify a proxied request.
	HeaderRequestID = "X-Inverting-Proxy-Request-ID"

	// HeaderRequestStartTime is the name of a response header used by the proxy
	// to report the start time of a proxied request.
	HeaderRequestStartTime = "X-Inverting-Proxy-Request-Start-Time"

	// JitterPercent sets the jitter for exponential backoff retry time
	JitterPercent = 0.1

	// Max time to wait before retry during exponential backoff
	maxBackoffDuration = 3 * time.Second

	// Time to wait on first retry
	firstRetryWaitDuration = time.Millisecond
)

var (
	// compute the max retry count
	maxRetryCount = math.Log2(float64(maxBackoffDuration / firstRetryWaitDuration))
)

// hopHeaders are Hop-by-hop headers. These are removed when received in a response from
// the backend. For details, see: http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true, // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true, // canonicalized version of "TE"
	"Trailer":             true, // not Trailers per URL above; http://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// PendingRequests represents a list of request IDs that do not yet have a response.
type PendingRequests []string

// ForwardedRequest represents an end-client HTTP request that was forwarded
// to us by the inverting proxy.
type ForwardedRequest struct {
	BackendID string
	RequestID string
	User      string
	StartTime time.Time

	Contents *http.Request
}

// CookieCache represents a LRU cache to store session ID -> CookieJar
type CookieCache struct {
	sessionCookieName    string
	sessionCookieTimeout time.Duration
	disableSSLForTest    bool

	cache *lru.Cache
	mu    sync.Mutex
}

// NewCookieCache initializes an LRU cache with cookieCacheLimit entries
func NewCookieCache(sessionCookieName string, sessionCookieTimeout time.Duration, cookieCacheLimit int, disableSSLForTest bool) (*CookieCache, error) {
	return &CookieCache{
		sessionCookieName:    sessionCookieName,
		sessionCookieTimeout: sessionCookieTimeout,
		disableSSLForTest:    disableSSLForTest,
		cache:                lru.New(cookieCacheLimit),
	}, nil
}

// AddJarToCache takes a Jar from http.Client and stores it in a cache
func (cj *CookieCache) AddJarToCache(sessionID string, jar http.CookieJar) {
	cj.mu.Lock()
	cj.cache.Add(sessionID, jar)
	cj.mu.Unlock()
}

// GetCachedCookieJar returns the CookieJar mapped to the sessionID
func (cj *CookieCache) GetCachedCookieJar(sessionID string) (jar http.CookieJar, err error) {
	val, ok := cj.cache.Get(sessionID)
	if !ok {
		options := cookiejar.Options{
			PublicSuffixList: publicsuffix.List,
		}
		jar, err = cookiejar.New(&options)
		cj.AddJarToCache(sessionID, jar)
		return jar, err
	}

	jar, ok = val.(http.CookieJar)
	if !ok {
		return nil, fmt.Errorf("Internal error; unexpected type for value (%+v) stored in the cookie jar cache", val)
	}
	return jar, nil
}

// InterceptSession modifies the given ResponseWriter by removing any Set-Cookie headers
// and instead adding those cookies to the corresponding session.
//
// If there is not already a session, and we have new cookies to save in the session, then
// this method will create a new session, and set a session cookie for it.
//
// This is the inverse of ExtractAndRestoreSession.
func (cj *CookieCache) InterceptSession(sessionID string, w http.ResponseWriter, u *url.URL) error {
	if cj == nil {
		return nil
	}

	header := w.Header()
	cookiesToAdd := (&http.Response{Header: header}).Cookies()
	if len(cookiesToAdd) == 0 {
		// There were no cookies to intercept
		return nil
	}

	header.Del("Set-Cookie")
	if sessionID == "" {
		// No session was previously defined, so we need to create a new one
		sessionID = uuid.New().String()
		sessionCookie := &http.Cookie{
			Name:     cj.sessionCookieName,
			Value:    sessionID,
			Path:     "/",
			Secure:   !cj.disableSSLForTest,
			HttpOnly: true,
			Expires:  time.Now().Add(cj.sessionCookieTimeout),
		}
		header.Add("Set-Cookie", sessionCookie.String())
	}

	cookieJar, err := cj.GetCachedCookieJar(sessionID)
	if err != nil {
		log.Printf("Failure reading a cached cookie jar: %v", err)
		return fmt.Errorf("Failure reading a cached cookie jar: %v", err)
	}
	cookieJar.SetCookies(u, cookiesToAdd)
	return nil
}

type SessionResponseWriter struct {
	cj            *CookieCache
	sessionID     string
	urlForCookies *url.URL

	wrapped     http.ResponseWriter
	wroteHeader bool
}

func (w *SessionResponseWriter) Header() http.Header {
	return w.wrapped.Header()
}

func (w *SessionResponseWriter) Write(bs []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.wrapped.Write(bs)
}

func (w *SessionResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		// Multiple calls ot WriteHeader are no-ops
		return
	}
	w.wroteHeader = true
	w.cj.InterceptSession(w.sessionID, w, w.urlForCookies)
	w.wrapped.WriteHeader(statusCode)
}

// ExtractAndRestoreSession pulls the session ID cookie (if any) out of the given request,
// finds the corresponding session, and then adds any saved cookies for that session to the request.
//
// The return value is the session ID, or an empty string if there is no session.
//
// This is the inverse of InterceptSession.
func (cj *CookieCache) ExtractAndRestoreSession(r *http.Request, u *url.URL) (sessionID string) {
	if cj == nil {
		return ""
	}

	sessionCookie, err := r.Cookie(cj.sessionCookieName)
	if err != nil || sessionCookie == nil {
		// There is no session cookie, so we have nothing to do.
		return ""
	}

	sessionID = sessionCookie.Value
	cachedCookieJar, err := cj.GetCachedCookieJar(sessionID)
	if err != nil {
		log.Printf("Failure reading the cookie jar for session %q: %v", sessionID, err)
		// We are unable to fetch a cookie jar for the session, so we have no
		// existing, cached cookies to insert into the request.
		return ""
	}

	// Remove the session cookie
	existingCookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range existingCookies {
		if c.Name != cj.sessionCookieName {
			r.AddCookie(c)
		}
	}

	// Restore any cached cookies from the session
	cachedCookies := cachedCookieJar.Cookies(u)
	for _, c := range cachedCookies {
		r.AddCookie(c)
	}
	return sessionID
}

type sessionHandler struct {
	cj      *CookieCache
	wrapped http.Handler
}

func (h *sessionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	urlForCookies := *(r.URL)
	urlForCookies.Scheme = "https"
	urlForCookies.Host = r.Host
	sessionID := h.cj.ExtractAndRestoreSession(r, &urlForCookies)
	w = &SessionResponseWriter{
		cj:            h.cj,
		sessionID:     sessionID,
		urlForCookies: &urlForCookies,
		wrapped:       w,
	}
	h.wrapped.ServeHTTP(w, r)
}

func (cj *CookieCache) SessionHandler(wrapped http.Handler) http.Handler {
	return &sessionHandler{
		cj:      cj,
		wrapped: wrapped,
	}
}

// RequestCallback defines how the caller of `ReadRequest` uses the request that was read.
//
// This is done as a callback so that the caller of `ReadRequest` does not have to remember
// to call `Close()` on the nested *http.Request object's body.
type RequestCallback func(client *http.Client, fr *ForwardedRequest) error

// parseRequestIDs takes a response from the proxy and parses any forwarded request IDs out of it.
func parseRequestIDs(response *http.Response) ([]string, error) {
	responseBody := &io.LimitedReader{
		R: response.Body,
		// If a response is larger than 1MB, then truncate it. This will result in an
		// failure to parse the result, but that is better than a potential OOM.
		//
		// Note that this shouldn't happen anyway, since a reasonable proxy server
		// should limit the size of a response to less than this. For instance, the
		// initial version of our proxy will never return a list of more than 100
		// request IDs.
		N: 1024 * 1024,
	}
	responseBytes, err := ioutil.ReadAll(responseBody)
	if err != nil {
		return nil, fmt.Errorf("Failed to read the forwarded request: %q\n", err.Error())
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Failed to list pending requests: %d, %q", response.StatusCode, responseBytes)
	}
	if len(responseBytes) <= 0 {
		return []string{}, nil
	}

	var requests []string
	if err := json.Unmarshal(responseBytes, &requests); err != nil {
		return nil, fmt.Errorf("Failed to parse the requests: %q\n", err.Error())
	}
	return requests, nil
}

func hasVMServiceAccount() bool {
	if !metadata.OnGCE() {
		return false
	}

	if _, err := metadata.Get("instance/service-accounts/default/email"); err != nil {
		return false
	}
	return true
}

func getVMID(audience string) string {
	for {
		idPath := fmt.Sprintf("instance/service-accounts/default/identity?format=full&audience=%s", audience)
		vmID, err := metadata.Get(idPath)
		if err == nil {
			return vmID
		}
		log.Printf("failure fetching a VM ID: %v", err)
	}
}

type vmTransport struct {
	wrapped http.RoundTripper

	// Protects the `currID` field below
	sync.Mutex
	currID string
}

func (t *vmTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.Lock()
	id := t.currID
	t.Unlock()
	r.Header.Add(HeaderVMID, id)
	return t.wrapped.RoundTrip(r)
}

// RoundTripperWithVMIdentity returns an http.RoundTripper that includes a GCE VM ID token in
// every outbound request. The token is fetched from the metadata server and
// stored in the 'X-Inverting-Proxy-VM-ID' header.
//
// This method relies on the Google Compute Engine functionality for verifying a VM's identity
// (https://cloud.google.com/compute/docs/instances/verifying-instance-identity), so it if this
// is not running inside of a Google Compute Engine VM, then it just returns the passed in RoundTripper.
func RoundTripperWithVMIdentity(ctx context.Context, wrapped http.RoundTripper, proxyURL string) http.RoundTripper {
	if !hasVMServiceAccount() {
		return wrapped
	}

	transport := &vmTransport{
		wrapped: wrapped,
		currID:  getVMID(proxyURL),
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				nextID := getVMID(proxyURL)
				transport.Lock()
				transport.currID = nextID
				transport.Unlock()
			}
		}
	}()
	return transport
}

// ListPendingRequests issues a single request to the proxy to ask for the IDs of pending requests.
func ListPendingRequests(client *http.Client, proxyHost, backendID string) ([]string, error) {
	proxyURL := proxyHost + PendingPath
	proxyReq, err := http.NewRequest(http.MethodGet, proxyURL, nil)
	if err != nil {
		return nil, err
	}
	proxyReq.Header.Add(HeaderBackendID, backendID)
	proxyResp, err := client.Do(proxyReq)
	if err != nil {
		return nil, fmt.Errorf("A proxy request failed: %q", err.Error())
	}
	defer proxyResp.Body.Close()
	return parseRequestIDs(proxyResp)
}

func parseRequestFromProxyResponse(backendID, requestID string, proxyResp *http.Response) (*ForwardedRequest, error) {
	user := proxyResp.Header.Get(HeaderUserID)
	startTimeStr := proxyResp.Header.Get(HeaderRequestStartTime)

	if proxyResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Error status while reading %q from the proxy", requestID)
	}

	startTime, err := time.Parse(time.RFC3339Nano, startTimeStr)
	if err != nil {
		return nil, err
	}

	contents, err := http.ReadRequest(bufio.NewReader(proxyResp.Body))
	if err != nil {
		return nil, err
	}
	return &ForwardedRequest{
		BackendID: backendID,
		RequestID: requestID,
		User:      user,
		StartTime: startTime,
		Contents:  contents,
	}, nil
}

// ReadRequest reads a forwarded client request from the inverting proxy.
//
// If the returned request is non-nil, then it is passed to the provided callback.
func ReadRequest(client *http.Client, proxyHost, backendID, requestID string, callback RequestCallback) error {
	proxyURL := proxyHost + RequestPath
	proxyReq, err := http.NewRequest(http.MethodGet, proxyURL, nil)
	if err != nil {
		return err
	}
	proxyReq.Header.Add(HeaderBackendID, backendID)
	proxyReq.Header.Add(HeaderRequestID, requestID)
	proxyResp, err := client.Do(proxyReq)
	if err != nil {
		return fmt.Errorf("A proxy request failed: %q", err.Error())
	}
	defer proxyResp.Body.Close()

	fr, err := parseRequestFromProxyResponse(backendID, requestID, proxyResp)
	if err != nil {
		return err
	}
	return callback(client, fr)
}

// ResponseForwarder implements http.ResponseWriter by dumping a wire-compatible
// representation of the response to 'proxyWriter' field.
//
// ResponseForwarder is used by the agent to forward a response from the backend
// target to the inverting proxy.
type ResponseForwarder struct {
	proxyWriter        *io.PipeWriter
	startedChan        chan struct{}
	responseBodyWriter *io.PipeWriter

	// wroteHeader is set when WriteHeader is called. It's used to ensure a
	// call to WriteHeader before the first call to Write.
	wroteHeader bool

	// response is synthesized using the backend target response. We use its Write
	// method as a convenience when forwarding the wire-representation received
	// by the backend target.
	response *http.Response

	// header is used to store the response headers prior to sending them.
	// This is separate from the headers in the response as it includes hop headers,
	// which will be filtered out before sending the response.
	header http.Header

	// proxyClientErrors is a channel where any errors issuing a client request to
	// the proxy server get written.
	//
	// This is eventually returned to the caller of the Close method.
	proxyClientErrors chan error

	// forwardingErrors is a channel where all errors forwarding the streamed
	// response from the backend to the proxy get written.
	//
	// This is eventually returned to the caller of the Close method.
	forwardingErrors chan error

	// writeErrors is a channel where all errors writing the streamed response
	// from the backend server get written.
	//
	// This is eventually returned to the caller of the Close method.
	writeErrors chan error
}

// NewResponseForwarder constructs a new ResponseForwarder that forwards to the
// given proxy for the specified request.
func NewResponseForwarder(client *http.Client, proxyHost, backendID, requestID string) (*ResponseForwarder, error) {
	// The contortions below support streaming.
	//
	// There are two pipes:
	// 1. proxyReader, proxyWriter: The io.PipeWriter for the HTTP POST to the inverting proxy.
	//       To this pipe, we write the full HTTP response from the backend target in HTTP
	//       wire-format form. (Status + Headers + Body + Trailers)
	//
	// 2. responseBodyReader, responseBodyWriter: This pipe corresponds to the response body
	//       from the backend target. To this pipe, we stream each read from backend target.
	proxyReader, proxyWriter := io.Pipe()
	startedChan := make(chan struct{}, 1)
	responseBodyReader, responseBodyWriter := io.Pipe()

	proxyURL := proxyHost + ResponsePath
	proxyReq, err := http.NewRequest(http.MethodPost, proxyURL, proxyReader)
	if err != nil {
		return nil, err
	}
	proxyReq.Header.Set(HeaderBackendID, backendID)
	proxyReq.Header.Set(HeaderRequestID, requestID)
	proxyReq.Header.Set("Content-Type", "text/plain")

	proxyClientErrChan := make(chan error, 100)
	forwardingErrChan := make(chan error, 100)
	writeErrChan := make(chan error, 100)
	go func() {
		// Wait until the response body has started being written
		// (for a non-empty response) or for the response to
		// be closed (for an empty response) before triggering
		// the proxy request round trip.
		//
		// This ensures that we do not fetch the bearer token
		// for the auth header until the last possible moment.
		// That, in turn. prevents a race condition where the
		// token expires between the header being generated
		// and the request being sent to the proxy.
		<-startedChan
		if _, err := client.Do(proxyReq); err != nil {
			proxyClientErrChan <- err
		}
		close(proxyClientErrChan)
	}()

	responseForwarder := &ResponseForwarder{
		response: &http.Response{
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       responseBodyReader,
		},
		wroteHeader:        false,
		header:             make(http.Header),
		proxyWriter:        proxyWriter,
		startedChan:        startedChan,
		responseBodyWriter: responseBodyWriter,
		proxyClientErrors:  proxyClientErrChan,
		forwardingErrors:   forwardingErrChan,
		writeErrors:        writeErrChan,
	}
	return responseForwarder, nil
}

func (rf *ResponseForwarder) notify() {
	if rf.startedChan != nil {
		rf.startedChan <- struct{}{}
		rf.startedChan = nil
	}
}

// Header implements the http.ResponseWriter interface.
func (rf *ResponseForwarder) Header() http.Header {
	return rf.header
}

// Write implements the http.ResponseWriter interface.
func (rf *ResponseForwarder) Write(buf []byte) (int, error) {
	// As in net/http, call WriteHeader if it has not yet been called
	// before the first call to Write.
	if !rf.wroteHeader {
		rf.WriteHeader(http.StatusOK)
	}
	rf.notify()
	count, err := rf.responseBodyWriter.Write(buf)
	if err != nil {
		rf.writeErrors <- err
	}
	return count, err
}

// WriteHeader implements the http.ResponseWriter interface.
func (rf *ResponseForwarder) WriteHeader(code int) {
	// As in net/http, ignore multiple calls to WriteHeader.
	if rf.wroteHeader {
		return
	}
	rf.wroteHeader = true

	for k, v := range rf.header {
		if _, ok := hopHeaders[k]; ok {
			continue
		}
		rf.response.Header[k] = v
	}
	rf.response.StatusCode = code
	rf.response.Status = http.StatusText(rf.response.StatusCode)
	// This will write the status and headers immediately and stream the
	// body using the pipes we've wired.
	go func() {
		defer rf.proxyWriter.Close()
		if err := rf.response.Write(rf.proxyWriter); err != nil {
			rf.forwardingErrors <- err

			// Normally, the end of this goroutine indicates
			// that the response.Body reader has returned an EOF,
			// which means that the corresponding writer has been
			// closed. However, that is not necessarily the case
			// if we hit an error in the call to `Write`.
			//
			// In this case, there may still be someone writing
			// to the pipe writer, but we will no longer be reading
			// anything from the corresponding reader. As such,
			// we signal that issue to any remaining writers.
			rf.response.Body.(*io.PipeReader).CloseWithError(err)
		}
		close(rf.forwardingErrors)
	}()
}

// Close signals that the response has been fully read from the backend server,
// waits for that response to be forwarded to the proxy, and then reports any
// errors that occured while forwarding the response.
func (rf *ResponseForwarder) Close() error {
	rf.notify()
	var errs []error
	if err := rf.responseBodyWriter.Close(); err != nil {
		errs = append(errs, err)
	}
	for err := range rf.proxyClientErrors {
		errs = append(errs, err)
	}
	for err := range rf.forwardingErrors {
		errs = append(errs, err)
	}
	close(rf.writeErrors)
	for err := range rf.writeErrors {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("Multiple errors closing pipe writers: %s", errs)
	}
	return nil
}

// ExponentialBackoffDuration gets time to wait before retry for exponential
// backoff
func ExponentialBackoffDuration(retryCount uint) time.Duration {
	var targetDuration time.Duration
	if retryCount > uint(maxRetryCount) {
		targetDuration = maxBackoffDuration
	} else {
		targetDuration = (1 << retryCount) * firstRetryWaitDuration
	}

	targetDuration = addJitter(targetDuration, JitterPercent)
	return targetDuration
}

func addJitter(duration time.Duration, jitterPercent float64) time.Duration {
	jitter := 1 - jitterPercent + rand.Float64()*(jitterPercent*2)
	return time.Duration(float64(duration.Nanoseconds())*jitter) * time.Nanosecond
}
