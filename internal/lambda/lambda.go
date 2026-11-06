// Package lambda runs the board on AWS Lambda behind a function URL, speaking Lambda's runtime API
// directly, so the same handler serves Lambda, a Pi and a container without an AWS library.
package lambda

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

// runtimePrefix is the path of the runtime API's invocation operations.
const runtimePrefix = "/2018-06-01/runtime/invocation/"

// fallbackTimeout is how long an invocation runs when Lambda sends no deadline.
const fallbackTimeout = 30 * time.Second

// knownMethods is the request methods Handle passes to the board; any other is answered 400.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// Event is the part of a function URL request (payload format 2.0) the board reads. A scheduled
// EventBridge event arrives with Source "aws.events" and no request.
type Event struct {
	// RawPath is the request's path, not yet unescaped.
	RawPath string `json:"rawPath"`

	// RawQueryString is the query, without its question mark.
	RawQueryString string `json:"rawQueryString"`

	// Cookies is the request's cookies, which Lambda keeps out of Headers.
	Cookies []string `json:"cookies"`

	// Headers is the request's headers, with repeated ones joined by commas.
	Headers map[string]string `json:"headers"`

	// Body is the request's body, in base64 when IsBase64Encoded.
	Body string `json:"body"`

	// IsBase64Encoded reports whether Body is base64.
	IsBase64Encoded bool `json:"isBase64Encoded"`

	// Source is "aws.events" for a scheduled event.
	Source string `json:"source"`

	// RequestContext is the function URL's domain and the request's method and source address.
	RequestContext struct {
		DomainName string `json:"domainName"`
		HTTP       struct {
			Method   string `json:"method"`
			SourceIP string `json:"sourceIp"`
		} `json:"http"`
	} `json:"requestContext"`
}

// Response is a function URL response.
type Response struct {
	// StatusCode is the HTTP status.
	StatusCode int `json:"statusCode"`

	// Headers is the response's headers, with repeated ones joined by commas.
	Headers map[string]string `json:"headers"`

	// Cookies is the Set-Cookie values, which a function URL takes apart from Headers.
	Cookies []string `json:"cookies,omitempty"`

	// Body is the response's body, in base64 when IsBase64Encoded.
	Body string `json:"body"`

	// IsBase64Encoded reports whether Body is base64.
	IsBase64Encoded bool `json:"isBase64Encoded"`
}

// Runtime takes invocations from Lambda's runtime API and answers them, one at a time.
type Runtime struct {
	// API is Lambda's runtime API address, from AWS_LAMBDA_RUNTIME_API.
	API string

	// Handler returns the board's handler for the domain the function URL serves. It is asked on each
	// request until it succeeds, never concurrently, since the domain is only known once one arrives.
	Handler func(domain string) (http.Handler, error)

	// Scheduled runs the board's sweeps when the hourly schedule invokes the function.
	Scheduled func(ctx context.Context) error

	// Logger records failures to answer Lambda; nil logs nothing.
	Logger *slog.Logger

	// Client talks to the runtime API; Run sets a default when it is nil.
	Client *http.Client

	// handler is the board's handler, once Handler has returned it.
	handler http.Handler
}

// Run answers invocations until ctx ends.
func (r *Runtime) Run(ctx context.Context) error {
	if r.Client == nil {
		r.Client = &http.Client{}
	}
	for ctx.Err() == nil {
		requestID, payload, deadline, err := r.next(ctx)
		if err != nil {
			return err
		}
		invocation, cancel := context.WithDeadline(ctx, deadline)
		response, err := r.invoke(invocation, payload)
		cancel()
		if err != nil {
			r.post(ctx, requestID+"/error", map[string]string{"errorMessage": err.Error(), "errorType": "BoardError"})
			continue
		}
		r.post(ctx, requestID+"/response", response)
	}
	return ctx.Err()
}

// next waits for the next invocation and returns its request ID, payload and deadline.
func (r *Runtime) next(ctx context.Context) (string, []byte, time.Time, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+r.API+runtimePrefix+"next", nil)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	response, err := r.Client.Do(request)
	if err != nil {
		return "", nil, time.Time{}, fmt.Errorf("lambda: waiting for the next invocation: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", nil, time.Time{}, fmt.Errorf("lambda: the runtime API answered %d", response.StatusCode)
	}
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	deadline := time.Now().Add(fallbackTimeout)
	var milliseconds int64
	if _, err := fmt.Sscan(response.Header.Get("Lambda-Runtime-Deadline-Ms"), &milliseconds); err == nil {
		deadline = time.UnixMilli(milliseconds)
	}
	return response.Header.Get("Lambda-Runtime-Aws-Request-Id"), payload, deadline, nil
}

// invoke answers an invocation, either a function URL request or the scheduled sweep. A panic is
// logged and becomes a 500 for a request or an error for anything else, so Run keeps going.
func (r *Runtime) invoke(ctx context.Context, payload []byte) (response any, err error) {
	var event Event
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if r.Logger != nil {
			r.Logger.Error("an invocation panicked", "panic", recovered, "stack", string(debug.Stack()))
		}
		if event.RequestContext.HTTP.Method != "" {
			response, err = statusResponse(http.StatusInternalServerError), nil
			return
		}
		response, err = nil, fmt.Errorf("lambda: the invocation panicked: %v", recovered)
	}()
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("lambda: the event is not JSON: %w", err)
	}
	if event.RequestContext.HTTP.Method == "" {
		if event.Source == "aws.events" && r.Scheduled != nil {
			return map[string]string{"swept": "true"}, r.Scheduled(ctx)
		}
		return nil, errors.New("lambda: an event that is neither a request nor the schedule")
	}
	if r.handler == nil {
		handler, err := r.Handler(event.RequestContext.DomainName)
		if err != nil {
			return nil, err
		}
		r.handler = handler
	}
	return Handle(ctx, r.handler, event), nil
}

// post sends value to the runtime API at path, logging a failure to deliver it.
func (r *Runtime) post(ctx context.Context, path string, value any) {
	body, _ := json.Marshal(value)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+r.API+runtimePrefix+path, bytes.NewReader(body))
	if err != nil {
		return
	}
	response, err := r.Client.Do(request)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Error("answering Lambda failed", "error", err)
		}
		return
	}
	response.Body.Close()
}

// Handle turns a function URL event into an ordinary request, serves it, and turns the result into a
// function URL response. The response body always travels as base64, so avatars arrive intact. An
// unknown method or a target that does not parse is answered 400, as net/http's server would.
func Handle(ctx context.Context, handler http.Handler, event Event) Response {
	body := []byte(event.Body)
	if event.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(event.Body)
		if err != nil {
			return statusResponse(http.StatusBadRequest)
		}
		body = decoded
	}
	target := event.RawPath
	if event.RawQueryString != "" {
		target += "?" + event.RawQueryString
	}
	method := event.RequestContext.HTTP.Method
	if !knownMethods[method] || !strings.HasPrefix(target, "/") || strings.ContainsFunc(target, isInvalidTargetRune) {
		return statusResponse(http.StatusBadRequest)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return statusResponse(http.StatusBadRequest)
	}
	// ParseRequestURI reads the target as a server does, without splitting off a fragment.
	if request.URL, err = url.ParseRequestURI(target); err != nil {
		return statusResponse(http.StatusBadRequest)
	}
	request.Host = event.RequestContext.DomainName
	request.RemoteAddr = event.RequestContext.HTTP.SourceIP + ":0"
	request.RequestURI = target
	for name, value := range event.Headers {
		request.Header.Set(name, value)
	}
	if len(event.Cookies) > 0 {
		request.Header.Set("Cookie", strings.Join(event.Cookies, "; "))
	}
	request.Header.Set("X-Forwarded-Proto", "https")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := Response{
		StatusCode:      recorder.Code,
		Headers:         map[string]string{},
		Body:            base64.StdEncoding.EncodeToString(recorder.Body.Bytes()),
		IsBase64Encoded: true,
	}
	for name, values := range recorder.Header() {
		if strings.EqualFold(name, "Set-Cookie") {
			response.Cookies = append(response.Cookies, values...)
			continue
		}
		response.Headers[name] = strings.Join(values, ", ")
	}
	return response
}

// isInvalidTargetRune reports whether r is a space or an ASCII control, which no request target holds.
func isInvalidTargetRune(r rune) bool {
	return r <= ' ' || r == 0x7f
}

// statusResponse returns a plain-text response holding only status and its text.
func statusResponse(status int) Response {
	body := fmt.Sprintf("%d %s", status, http.StatusText(status))
	return Response{
		StatusCode:      status,
		Headers:         map[string]string{"Content-Type": "text/plain; charset=utf-8", "X-Content-Type-Options": "nosniff"},
		Body:            base64.StdEncoding.EncodeToString([]byte(body)),
		IsBase64Encoded: true,
	}
}
