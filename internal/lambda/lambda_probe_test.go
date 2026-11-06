package lambda_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/lambda"
)

// seen is what a probe handler observed of its request.
type seen struct {
	// request is the request as the handler received it.
	request *http.Request

	// body is the request body the handler read.
	body []byte

	// contextDone reports whether the request's context ended during the handler.
	contextDone bool
}

// recordingHandler stores the request it receives into target and answers 200 "ok".
func recordingHandler(target *seen) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		target.request = request
		target.body, _ = io.ReadAll(request.Body)
		select {
		case <-request.Context().Done():
			target.contextDone = true
		default:
		}
		writer.Write([]byte("ok"))
	})
}

// getEvent builds a GET event for path and query from a client at 203.0.113.9.
func getEvent(path, query string) lambda.Event {
	event := lambda.Event{RawPath: path, RawQueryString: query, Headers: map[string]string{}}
	event.RequestContext.DomainName = "abc.lambda-url.us-east-1.on.aws"
	event.RequestContext.HTTP.Method = "GET"
	event.RequestContext.HTTP.SourceIP = "203.0.113.9"
	return event
}

// decodeBody returns the response body bytes, decoding base64 when flagged.
func decodeBody(t *testing.T, response lambda.Response) []byte {
	t.Helper()
	if !response.IsBase64Encoded {
		return []byte(response.Body)
	}
	data, err := base64.StdEncoding.DecodeString(response.Body)
	if err != nil {
		t.Fatalf("the body is not base64: %v", err)
	}
	return data
}

// TestHandleTargetAndQuery passes the escaped path and query through so the signature's target matches.
func TestHandleTargetAndQuery(t *testing.T) {
	var got seen
	event := getEvent("/a%20b/c", "x=1&y=a%20b")
	response := lambda.Handle(context.Background(), recordingHandler(&got), event)
	if response.StatusCode != 200 {
		t.Fatalf("answered %d", response.StatusCode)
	}
	request := got.request
	if request.URL.Path != "/a b/c" {
		t.Errorf("URL.Path is %q, want \"/a b/c\"", request.URL.Path)
	}
	if request.URL.RawPath != "/a%20b/c" && request.URL.EscapedPath() != "/a%20b/c" {
		t.Errorf("EscapedPath is %q, want \"/a%%20b/c\"", request.URL.EscapedPath())
	}
	if request.URL.RawQuery != "x=1&y=a%20b" {
		t.Errorf("RawQuery is %q", request.URL.RawQuery)
	}
	if request.RequestURI != "/a%20b/c?x=1&y=a%20b" {
		t.Errorf("RequestURI is %q, want the raw path and query joined", request.RequestURI)
	}
	if request.URL.Query().Get("y") != "a b" {
		t.Errorf("query y is %q", request.URL.Query().Get("y"))
	}
}

// TestHandleEmptyQueryHasNoQuestionMark builds the RequestURI without a trailing question mark.
func TestHandleEmptyQueryHasNoQuestionMark(t *testing.T) {
	var got seen
	lambda.Handle(context.Background(), recordingHandler(&got), getEvent("/api/v1/board", ""))
	if got.request.RequestURI != "/api/v1/board" {
		t.Errorf("RequestURI is %q, want \"/api/v1/board\"", got.request.RequestURI)
	}
	if got.request.URL.RawQuery != "" || got.request.URL.ForceQuery {
		t.Errorf("an empty query became %q (ForceQuery %v)", got.request.URL.RawQuery, got.request.URL.ForceQuery)
	}
}

// TestHandleHeadersCookiesHostAndPeer copies headers, rebuilds cookies, and sets Host and RemoteAddr.
func TestHandleHeadersCookiesHostAndPeer(t *testing.T) {
	var got seen
	event := getEvent("/", "")
	event.Headers = map[string]string{"content-type": "application/json", "x-custom": "a,b", "authorization": "FitHalo-ECDSA member=x"}
	event.Cookies = []string{"session=abc", "lang=de"}
	lambda.Handle(context.Background(), recordingHandler(&got), event)
	request := got.request
	if request.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type is %q (case-insensitive lookup)", request.Header.Get("Content-Type"))
	}
	if request.Header.Get("X-Custom") != "a,b" || request.Header.Get("Authorization") != "FitHalo-ECDSA member=x" {
		t.Errorf("headers are %v", request.Header)
	}
	if cookie, err := request.Cookie("session"); err != nil || cookie.Value != "abc" {
		t.Errorf("cookie session: %v, %v", cookie, err)
	}
	if cookie, err := request.Cookie("lang"); err != nil || cookie.Value != "de" {
		t.Errorf("cookie lang: %v, %v", cookie, err)
	}
	if request.Host != "abc.lambda-url.us-east-1.on.aws" {
		t.Errorf("Host is %q", request.Host)
	}
	host := request.RemoteAddr
	if index := strings.LastIndex(host, ":"); index > 0 && !strings.Contains(host, "]") || strings.Count(host, ":") == 1 {
		host = host[:strings.LastIndex(host, ":")]
	}
	if host != "203.0.113.9" {
		t.Errorf("RemoteAddr is %q, want the request context's source address", request.RemoteAddr)
	}
	t.Logf("RemoteAddr is %q", request.RemoteAddr)
}

// TestHandleForcesForwardedProto sets X-Forwarded-Proto https whatever the client sent.
func TestHandleForcesForwardedProto(t *testing.T) {
	for _, sent := range []string{"", "http", "HTTP", "ftp", "https, http"} {
		var got seen
		event := getEvent("/", "")
		if sent != "" {
			event.Headers["x-forwarded-proto"] = sent
		}
		lambda.Handle(context.Background(), recordingHandler(&got), event)
		if values := got.request.Header.Values("X-Forwarded-Proto"); len(values) != 1 || values[0] != "https" {
			t.Errorf("client sent X-Forwarded-Proto %q; the handler sees %v, want [https]", sent, values)
		}
	}
}

// TestHandleSpoofedForwardedForDoesNotBecomePeer keeps RemoteAddr from the request context.
func TestHandleSpoofedForwardedForDoesNotBecomePeer(t *testing.T) {
	var got seen
	event := getEvent("/", "")
	event.Headers["x-forwarded-for"] = "1.1.1.1"
	lambda.Handle(context.Background(), recordingHandler(&got), event)
	if strings.HasPrefix(got.request.RemoteAddr, "1.1.1.1") {
		t.Errorf("RemoteAddr %q came from the client's X-Forwarded-For", got.request.RemoteAddr)
	}
	// PROBE: the client's X-Forwarded-For reaches the handler untouched. The context's sourceIp is the
	// truth on Lambda; whichever address function the Lambda main wires must read RemoteAddr, not
	// serving.ProxyClientAddress, or a client picks its own rate-limit bucket.
	t.Logf("the handler sees X-Forwarded-For %q and RemoteAddr %q", got.request.Header.Get("X-Forwarded-For"), got.request.RemoteAddr)
}

// TestHandleBodyEncodings decodes base64 bodies, passes plain ones, and sets Content-Length.
func TestHandleBodyEncodings(t *testing.T) {
	binary := make([]byte, 3000)
	rand.New(rand.NewSource(1)).Read(binary)
	cases := []struct {
		// name names the case.
		name string

		// body is the event body.
		body string

		// encoded flags base64.
		encoded bool

		// want is the body the handler must read.
		want []byte
	}{
		{"plain", `{"a":1}`, false, []byte(`{"a":1}`)},
		{"base64 text", base64.StdEncoding.EncodeToString([]byte(`{"a":1}`)), true, []byte(`{"a":1}`)},
		{"base64 binary", base64.StdEncoding.EncodeToString(binary), true, binary},
		{"empty plain", "", false, []byte{}},
		{"empty base64", "", true, []byte{}},
		{"plain utf8", "héllo wörld ✓", false, []byte("héllo wörld ✓")},
	}
	for _, c := range cases {
		var got seen
		event := getEvent("/", "")
		event.RequestContext.HTTP.Method = "POST"
		event.Body, event.IsBase64Encoded = c.body, c.encoded
		response := lambda.Handle(context.Background(), recordingHandler(&got), event)
		if response.StatusCode != 200 {
			t.Errorf("%s: answered %d", c.name, response.StatusCode)
			continue
		}
		if !bytes.Equal(got.body, c.want) {
			t.Errorf("%s: the handler read %d bytes, want %d", c.name, len(got.body), len(c.want))
		}
		if got.request.ContentLength != int64(len(c.want)) {
			t.Errorf("%s: ContentLength is %d, want %d", c.name, got.request.ContentLength, len(c.want))
		}
	}
}

// TestHandleInvalidBase64IsRefused answers 400 to a body flagged base64 that does not decode.
func TestHandleInvalidBase64IsRefused(t *testing.T) {
	for _, body := range []string{"!!!!", "abc", "YWJj\x00", "YW Jj"} {
		var got seen
		event := getEvent("/", "")
		event.RequestContext.HTTP.Method = "POST"
		event.Body, event.IsBase64Encoded = body, true
		response := lambda.Handle(context.Background(), recordingHandler(&got), event)
		if response.StatusCode != 400 {
			t.Errorf("base64 body %q answered %d, want 400 (the handler read %q)", body, response.StatusCode, got.body)
		}
	}
}

// TestHandleResponseShape base64-encodes the body, joins headers, and moves Set-Cookie to Cookies.
func TestHandleResponseShape(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Add("Vary", "Accept-Encoding")
		writer.Header().Add("Vary", "Accept-Language")
		http.SetCookie(writer, &http.Cookie{Name: "session", Value: "abc", HttpOnly: true, Path: "/admin"})
		http.SetCookie(writer, &http.Cookie{Name: "lang", Value: "de"})
		writer.WriteHeader(201)
		writer.Write([]byte(`{"id":1}`))
	})
	response := lambda.Handle(context.Background(), handler, getEvent("/", ""))
	if response.StatusCode != 201 {
		t.Errorf("StatusCode %d", response.StatusCode)
	}
	if !response.IsBase64Encoded {
		t.Error("the body is not flagged base64")
	}
	if string(decodeBody(t, response)) != `{"id":1}` {
		t.Errorf("body is %q", decodeBody(t, response))
	}
	if len(response.Cookies) != 2 || !strings.HasPrefix(response.Cookies[0], "session=abc") || !strings.HasPrefix(response.Cookies[1], "lang=de") {
		t.Errorf("Cookies are %v", response.Cookies)
	}
	for name := range response.Headers {
		if strings.EqualFold(name, "Set-Cookie") {
			t.Errorf("Set-Cookie is still in Headers: %v", response.Headers)
		}
	}
	vary := ""
	for name, value := range response.Headers {
		if strings.EqualFold(name, "Vary") {
			vary = value
		}
	}
	if vary != "Accept-Encoding,Accept-Language" && vary != "Accept-Encoding, Accept-Language" {
		t.Errorf("repeated Vary became %q", vary)
	}
}

// TestHandleBinaryBodySurvives round-trips 10 KB of random bytes.
func TestHandleBinaryBodySurvives(t *testing.T) {
	body := make([]byte, 10*1024)
	rand.New(rand.NewSource(7)).Read(body)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "image/jpeg")
		writer.Write(body)
	})
	response := lambda.Handle(context.Background(), handler, getEvent("/avatar", ""))
	if !bytes.Equal(decodeBody(t, response), body) {
		t.Error("the binary body changed")
	}
}

// TestHandleEmptyBodyAnd304 answers an empty body and a 304 cleanly.
func TestHandleEmptyBodyAnd304(t *testing.T) {
	for _, status := range []int{200, 204, 304} {
		handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("ETag", `"abc"`)
			writer.WriteHeader(status)
		})
		response := lambda.Handle(context.Background(), handler, getEvent("/", ""))
		if response.StatusCode != status {
			t.Errorf("%d became %d", status, response.StatusCode)
		}
		if len(decodeBody(t, response)) != 0 {
			t.Errorf("%d carried a body %q", status, response.Body)
		}
		if response.Headers["ETag"] != `"abc"` && response.Headers["Etag"] != `"abc"` && response.Headers["etag"] != `"abc"` {
			t.Errorf("%d lost the ETag: %v", status, response.Headers)
		}
	}
}

// TestHandleImplicitStatus reports 200 when the handler never calls WriteHeader.
func TestHandleImplicitStatus(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.Write([]byte("x")) })
	if response := lambda.Handle(context.Background(), handler, getEvent("/", "")); response.StatusCode != 200 {
		t.Errorf("implicit status is %d", response.StatusCode)
	}
	silent := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if response := lambda.Handle(context.Background(), silent, getEvent("/", "")); response.StatusCode != 200 {
		t.Errorf("a handler writing nothing answered %d", response.StatusCode)
	}
}

// TestHandlePanicPropagates records that Handle itself does not recover a handler's panic.
func TestHandlePanicPropagates(t *testing.T) {
	// PROBE: Handle lets the panic through and Run recovers it (TestRunPanickingHandlerIsAnswered500).
	// The exported doc promises neither; any other caller of Handle crashes on a handler panic.
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	recovered := func() (result any) {
		defer func() { result = recover() }()
		response := lambda.Handle(context.Background(), handler, getEvent("/", ""))
		t.Logf("Handle returned %d for a panicking handler", response.StatusCode)
		return nil
	}()
	t.Logf("Handle propagated: %v", recovered)
}

// TestHandleRefusesBadMethodsAndTargets answers 400 to what net/http's server would refuse.
func TestHandleRefusesBadMethodsAndTargets(t *testing.T) {
	cases := []struct {
		// name names the case.
		name string

		// method is the event's method.
		method string

		// path is the raw path.
		path string

		// query is the raw query.
		query string
	}{
		{"method with target", "GET /", "/", ""},
		{"empty method", "", "/", ""},
		{"method with space", "G ET", "/", ""},
		{"method lowercase", "get", "/", ""},
		{"method with newline", "GET\n", "/", ""},
		{"path with cr", "GET", "/a\rb", ""},
		{"path with lf", "GET", "/a\nb", ""},
		{"path with nul", "GET", "/a\x00b", ""},
		{"path with tab", "GET", "/a\tb", ""},
		{"path with bad escape", "GET", "/%zz", ""},
		{"path with short escape", "GET", "/%2", ""},
		{"relative path", "GET", "board", ""},
		{"empty path", "GET", "", ""},
		{"query with crlf", "GET", "/", "a=1\r\nHost: evil"},
		{"query with space", "GET", "/", "a=1 b"},
		{"path with hash", "GET", "/a#b", ""},
		{"path with question mark", "GET", "/a?b=1", ""},
		{"absolute url", "GET", "http://evil/x", ""},
		{"scheme relative", "GET", "//evil/x", ""},
		{"unicode path", "GET", "/ሴ", ""},
	}
	for _, c := range cases {
		served := false
		handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })
		event := getEvent(c.path, c.query)
		event.RequestContext.HTTP.Method = c.method
		response := lambda.Handle(context.Background(), handler, event)
		switch c.name {
		case "unicode path", "scheme relative", "path with hash", "path with question mark":
			// PROBE: net/http's own parser accepts these too: a raw UTF-8 path, "//evil/x" as a path with an
			// empty host, '#' as a path character, and '?' splitting the query. Lambda never sends them.
			t.Logf("%s: %q %q answered %d, served %v", c.name, c.method, c.path, response.StatusCode, served)
			continue
		}
		if response.StatusCode != 400 {
			t.Errorf("%s: %q %q?%q answered %d, want 400", c.name, c.method, c.path, c.query, response.StatusCode)
		}
		if served {
			t.Errorf("%s: %q %q?%q reached the handler", c.name, c.method, c.path, c.query)
		}
	}
}

// TestHandleAcceptsEveryStandardMethod passes the methods the API uses.
func TestHandleAcceptsEveryStandardMethod(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"} {
		var got seen
		event := getEvent("/", "")
		event.RequestContext.HTTP.Method = method
		response := lambda.Handle(context.Background(), recordingHandler(&got), event)
		if response.StatusCode != 200 || got.request == nil || got.request.Method != method {
			t.Errorf("%s answered %d (handler saw %v)", method, response.StatusCode, got.request != nil)
		}
	}
}

// TestHandleLargeResponse records how a body over Lambda's 6 MB payload limit is answered.
func TestHandleLargeResponse(t *testing.T) {
	// PROBE: a function URL refuses a response payload over 6 MB; base64 inflates 5 MB to 6.7 MB.
	// The board's bodies are far smaller (the avatar is 512 × 512 JPEG), so this only records the shape.
	large := bytes.Repeat([]byte("x"), 5*1024*1024)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.Write(large) })
	response := lambda.Handle(context.Background(), handler, getEvent("/", ""))
	t.Logf("a 5 MB body answered %d with a %d-byte base64 body", response.StatusCode, len(response.Body))
	if response.StatusCode == 200 && !bytes.Equal(decodeBody(t, response), large) {
		t.Error("the 5 MB body changed")
	}
}

// TestHandleContextReachesHandler passes ctx so a cancelled invocation is visible to the handler.
func TestHandleContextReachesHandler(t *testing.T) {
	type keyType struct{}
	ctx := context.WithValue(context.Background(), keyType{}, "marker")
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	var got seen
	lambda.Handle(ctx, recordingHandler(&got), getEvent("/", ""))
	if got.request == nil {
		t.Fatal("the handler did not run")
	}
	if got.request.Context().Value(keyType{}) != "marker" {
		t.Error("the handler's context is not derived from ctx")
	}
	if !got.contextDone {
		t.Error("a cancelled ctx is not visible to the handler")
	}
}

// TestHandleHeaderNamesAreCanonical lets the handler read mixed-case event header names.
func TestHandleHeaderNamesAreCanonical(t *testing.T) {
	var got seen
	event := getEvent("/", "")
	event.Headers = map[string]string{"If-None-Match": `"abc"`, "ACCEPT-ENCODING": "gzip", "x-amzn-trace-id": "Root=1"}
	lambda.Handle(context.Background(), recordingHandler(&got), event)
	if got.request.Header.Get("If-None-Match") != `"abc"` || got.request.Header.Get("Accept-Encoding") != "gzip" || got.request.Header.Get("X-Amzn-Trace-Id") != "Root=1" {
		t.Errorf("headers are %v", got.request.Header)
	}
}

// TestHandleHostHeaderFromClientIsIgnored sets Host from the request context, not the event's host header.
func TestHandleHostHeaderFromClientIsIgnored(t *testing.T) {
	var got seen
	event := getEvent("/", "")
	event.Headers["host"] = "evil.example"
	lambda.Handle(context.Background(), recordingHandler(&got), event)
	if got.request.Host != "abc.lambda-url.us-east-1.on.aws" {
		t.Errorf("Host is %q, want the function URL's domain", got.request.Host)
	}
}

// TestHandleConcurrent serves many events at once.
func TestHandleConcurrent(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		writer.Write(body)
	})
	var group sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			event := getEvent("/", "")
			event.RequestContext.HTTP.Method = "POST"
			event.Body = strconv.Itoa(worker)
			response := lambda.Handle(context.Background(), handler, event)
			if string(decodeBody(t, response)) != strconv.Itoa(worker) {
				t.Errorf("worker %d got %q", worker, response.Body)
			}
		}(worker)
	}
	group.Wait()
}

// runtimePrefix is the runtime API's path prefix.
const runtimePrefix = "/2018-06-01/runtime/"

// fakeLambda serves Lambda's runtime API: it hands out events and records what is posted back.
type fakeLambda struct {
	// mutex guards every field.
	mutex sync.Mutex

	// events is the payloads still to hand out.
	events [][]byte

	// nextStatuses is a queue of status codes to answer /next with before handing out events.
	nextStatuses []int

	// posts records each POST body by its path after the prefix.
	posts map[string]string

	// order records the paths posted to, in order.
	order []string

	// handed counts the events handed out, to number request ids.
	handed int
}

// newFakeLambda returns a fake with events queued.
func newFakeLambda(events ...[]byte) *fakeLambda {
	return &fakeLambda{events: events, posts: map[string]string{}}
}

// ServeHTTP answers /next with the next event, blocking when none is left, and records posts.
func (f *fakeLambda) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mutex.Lock()
	if strings.HasSuffix(request.URL.Path, "/invocation/next") {
		if len(f.nextStatuses) > 0 {
			status := f.nextStatuses[0]
			f.nextStatuses = f.nextStatuses[1:]
			f.mutex.Unlock()
			writer.WriteHeader(status)
			return
		}
		if len(f.events) == 0 {
			f.mutex.Unlock()
			<-request.Context().Done()
			return
		}
		f.handed++
		id := "request-" + strconv.Itoa(f.handed)
		event := f.events[0]
		f.events = f.events[1:]
		f.mutex.Unlock()
		writer.Header().Set("Lambda-Runtime-Aws-Request-Id", id)
		writer.Header().Set("Lambda-Runtime-Deadline-Ms", strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10))
		writer.Header().Set("Lambda-Runtime-Invoked-Function-Arn", "arn:aws:lambda:us-east-1:123456789012:function:board")
		writer.Write(event)
		return
	}
	defer f.mutex.Unlock()
	body, _ := io.ReadAll(request.Body)
	path := strings.TrimPrefix(request.URL.Path, runtimePrefix)
	f.posts[path] = string(body)
	f.order = append(f.order, request.Method+" "+path)
	writer.WriteHeader(http.StatusAccepted)
	writer.Write([]byte(`{"status":"OK"}`))
}

// snapshot returns copies of the posts and their order.
func (f *fakeLambda) snapshot() (map[string]string, []string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	posts := map[string]string{}
	for key, value := range f.posts {
		posts[key] = value
	}
	return posts, append([]string{}, f.order...)
}

// countPosts returns how many posts have been recorded.
func (f *fakeLambda) countPosts() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.order)
}

// waitForPosts waits until the fake has at least n posts or the deadline passes.
func (f *fakeLambda) waitForPosts(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.countPosts() < n {
		if time.Now().After(deadline) {
			_, order := f.snapshot()
			t.Fatalf("only %d posts arrived: %v", len(order), order)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// runUntilPosts runs runtime against fake until n posts arrive, then cancels and returns Run's error.
func runUntilPosts(t *testing.T, fake *fakeLambda, runtime *lambda.Runtime, n int) error {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	runtime.API = strings.TrimPrefix(server.URL, "http://")
	runtime.Client = server.Client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	fake.waitForPosts(t, n)
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
		return nil
	}
}

// eventJSON marshals an event.
func eventJSON(t *testing.T, event lambda.Event) []byte {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// echoHandler answers each request with its path.
var echoHandler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
	writer.Write([]byte(request.URL.Path))
})

// TestRunAnswersEachInvocationByID posts each answer to its own request id with the right body.
func TestRunAnswersEachInvocationByID(t *testing.T) {
	fake := newFakeLambda(eventJSON(t, getEvent("/one", "")), eventJSON(t, getEvent("/two", "")), eventJSON(t, getEvent("/three", "")))
	runtime := &lambda.Runtime{
		Handler:   func(string) (http.Handler, error) { return echoHandler, nil },
		Scheduled: func(context.Context) error { return nil },
	}
	err := runUntilPosts(t, fake, runtime, 3)
	posts, order := fake.snapshot()
	t.Logf("Run returned %v after ctx was cancelled; posts in order: %v", err, order)
	for index, path := range []string{"/one", "/two", "/three"} {
		key := "invocation/request-" + strconv.Itoa(index+1) + "/response"
		var response lambda.Response
		if err := json.Unmarshal([]byte(posts[key]), &response); err != nil {
			t.Fatalf("%s: %v (%q)", key, err, posts[key])
		}
		if response.StatusCode != 200 || string(decodeBody(t, response)) != path {
			t.Errorf("%s answered %d %q, want 200 %q", key, response.StatusCode, decodeBody(t, response), path)
		}
	}
	if len(order) != 3 {
		t.Errorf("%d posts, want 3: %v", len(order), order)
	}
}

// TestRunScheduledEvent calls Scheduled for an EventBridge event and posts a response.
func TestRunScheduledEvent(t *testing.T) {
	fake := newFakeLambda([]byte(`{"version":"0","source":"aws.events","detail-type":"Scheduled Event","detail":{}}`))
	swept := 0
	runtime := &lambda.Runtime{
		Handler: func(string) (http.Handler, error) {
			t.Error("the handler was asked for a scheduled event")
			return echoHandler, nil
		},
		Scheduled: func(context.Context) error { swept++; return nil },
	}
	runUntilPosts(t, fake, runtime, 1)
	posts, order := fake.snapshot()
	if swept != 1 {
		t.Errorf("Scheduled ran %d times", swept)
	}
	if _, ok := posts["invocation/request-1/response"]; !ok {
		t.Errorf("the scheduled event was not answered with a response: %v", order)
	}
}

// TestRunScheduledErrorIsReported posts an error when Scheduled fails, and goes on.
func TestRunScheduledErrorIsReported(t *testing.T) {
	fake := newFakeLambda([]byte(`{"source":"aws.events"}`), eventJSON(t, getEvent("/after", "")))
	runtime := &lambda.Runtime{
		Handler:   func(string) (http.Handler, error) { return echoHandler, nil },
		Scheduled: func(context.Context) error { return errors.New("sweep failed") },
	}
	runUntilPosts(t, fake, runtime, 2)
	posts, order := fake.snapshot()
	t.Logf("posts: %v", order)
	if _, ok := posts["invocation/request-1/error"]; !ok {
		t.Errorf("a failed sweep was not posted as an error: %v", order)
	}
	if _, ok := posts["invocation/request-2/response"]; !ok {
		t.Errorf("the loop did not go on after a failed sweep: %v", order)
	}
}

// TestRunRetriesHandlerConstruction asks Handler again after it fails, without caching the failure.
func TestRunRetriesHandlerConstruction(t *testing.T) {
	fake := newFakeLambda(eventJSON(t, getEvent("/first", "")), eventJSON(t, getEvent("/second", "")), eventJSON(t, getEvent("/third", "")))
	asked := 0
	runtime := &lambda.Runtime{
		Handler: func(domain string) (http.Handler, error) {
			asked++
			if asked == 1 {
				return nil, errors.New("store not ready")
			}
			return echoHandler, nil
		},
		Scheduled: func(context.Context) error { return nil },
	}
	runUntilPosts(t, fake, runtime, 3)
	posts, order := fake.snapshot()
	t.Logf("posts: %v", order)
	if asked != 2 {
		t.Errorf("Handler was asked %d times, want 2 (once failing, once succeeding, then cached)", asked)
	}
	if _, ok := posts["invocation/request-1/error"]; !ok {
		if response, ok := posts["invocation/request-1/response"]; ok {
			var parsed lambda.Response
			json.Unmarshal([]byte(response), &parsed)
			if parsed.StatusCode < 500 {
				t.Errorf("the first invocation, whose handler failed to build, was answered %d", parsed.StatusCode)
			}
		} else {
			t.Errorf("the first invocation was not answered at all: %v", order)
		}
	}
	for _, id := range []string{"request-2", "request-3"} {
		var response lambda.Response
		json.Unmarshal([]byte(posts["invocation/"+id+"/response"]), &response)
		if response.StatusCode != 200 {
			t.Errorf("%s answered %d after the handler was built", id, response.StatusCode)
		}
	}
}

// TestRunMalformedEventIsAnsweredAndLoopGoesOn posts an error for unparsable JSON and continues.
func TestRunMalformedEventIsAnsweredAndLoopGoesOn(t *testing.T) {
	fake := newFakeLambda([]byte(`{not json`), eventJSON(t, getEvent("/after", "")))
	runtime := &lambda.Runtime{
		Handler:   func(string) (http.Handler, error) { return echoHandler, nil },
		Scheduled: func(context.Context) error { return nil },
	}
	runUntilPosts(t, fake, runtime, 2)
	posts, order := fake.snapshot()
	t.Logf("posts: %v", order)
	if _, ok := posts["invocation/request-1/error"]; !ok {
		if _, ok := posts["invocation/request-1/response"]; !ok {
			t.Errorf("the malformed event was not answered: %v", order)
		} else {
			t.Logf("the malformed event was answered with a response rather than an error")
		}
	}
	var response lambda.Response
	json.Unmarshal([]byte(posts["invocation/request-2/response"]), &response)
	if response.StatusCode != 200 {
		t.Errorf("after a malformed event the next was answered %d", response.StatusCode)
	}
}

// TestRunPanickingHandlerIsAnswered500 answers 500 and continues when the handler panics.
func TestRunPanickingHandlerIsAnswered500(t *testing.T) {
	fake := newFakeLambda(eventJSON(t, getEvent("/panic", "")), eventJSON(t, getEvent("/fine", "")))
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/panic" {
			panic("boom")
		}
		writer.Write([]byte("fine"))
	})
	runtime := &lambda.Runtime{
		Handler:   func(string) (http.Handler, error) { return handler, nil },
		Scheduled: func(context.Context) error { return nil },
	}
	runUntilPosts(t, fake, runtime, 2)
	posts, _ := fake.snapshot()
	var first, second lambda.Response
	json.Unmarshal([]byte(posts["invocation/request-1/response"]), &first)
	json.Unmarshal([]byte(posts["invocation/request-2/response"]), &second)
	if first.StatusCode != 500 {
		t.Errorf("the panicking request was answered %d: %v", first.StatusCode, posts)
	}
	if second.StatusCode != 200 || string(decodeBody(t, second)) != "fine" {
		t.Errorf("the request after the panic was answered %d", second.StatusCode)
	}
}

// TestRunScheduledEventWithMethodIsNotASweep refuses a function URL caller posing as the schedule.
func TestRunScheduledEventWithMethodIsNotASweep(t *testing.T) {
	// plans/reference-security-review.md: the sweep requires an EventBridge source and no HTTP method.
	event := getEvent("/", "")
	event.Source = "aws.events"
	fake := newFakeLambda(eventJSON(t, event))
	swept := 0
	runtime := &lambda.Runtime{
		Handler:   func(string) (http.Handler, error) { return echoHandler, nil },
		Scheduled: func(context.Context) error { swept++; return nil },
	}
	runUntilPosts(t, fake, runtime, 1)
	if swept != 0 {
		t.Error("a request carrying source aws.events ran the sweep")
	}
}

// TestRunSurvivesNextFailures keeps polling when /next answers 500, then serves the event.
func TestRunSurvivesNextFailures(t *testing.T) {
	// PROBE: Lambda's runtime API should not fail /next; record whether Run retries or exits.
	fake := newFakeLambda(eventJSON(t, getEvent("/after", "")))
	fake.nextStatuses = []int{500, 503}
	runtime := &lambda.Runtime{
		Handler:   func(string) (http.Handler, error) { return echoHandler, nil },
		Scheduled: func(context.Context) error { return nil },
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	runtime.API = strings.TrimPrefix(server.URL, "http://")
	runtime.Client = server.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case err := <-done:
		_, order := fake.snapshot()
		t.Logf("Run returned %v with posts %v", err, order)
		if len(order) == 0 && ctx.Err() == nil {
			t.Log("Run exits when /next fails rather than retrying")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestRunReturnsWhenContextEnds returns promptly once ctx is cancelled while waiting on /next.
func TestRunReturnsWhenContextEnds(t *testing.T) {
	fake := newFakeLambda()
	runtime := &lambda.Runtime{
		Handler:   func(string) (http.Handler, error) { return echoHandler, nil },
		Scheduled: func(context.Context) error { return nil },
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	runtime.API = strings.TrimPrefix(server.URL, "http://")
	runtime.Client = server.Client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		t.Logf("Run returned %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}

// TestRunPostsValidJSONShape posts a response whose JSON has the function URL fields.
func TestRunPostsValidJSONShape(t *testing.T) {
	fake := newFakeLambda(eventJSON(t, getEvent("/x", "")))
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.SetCookie(writer, &http.Cookie{Name: "a", Value: "b"})
		writer.Header().Set("Content-Type", "text/plain")
		writer.Write([]byte("x"))
	})
	runtime := &lambda.Runtime{
		Handler:   func(string) (http.Handler, error) { return handler, nil },
		Scheduled: func(context.Context) error { return nil },
	}
	runUntilPosts(t, fake, runtime, 1)
	posts, _ := fake.snapshot()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(posts["invocation/request-1/response"]), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"statusCode", "headers", "body", "isBase64Encoded", "cookies"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("the posted response lacks %s: %s", field, posts["invocation/request-1/response"])
		}
	}
}
