package lambda

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHandleCarriesTheRequest checks the event becomes the request the board expects, and the response
// goes back with its cookies apart from its headers.
func TestHandleCarriesTheRequest(t *testing.T) {
	var seen *http.Request
	var seenBody []byte
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = request
		seenBody, _ = io.ReadAll(request.Body)
		http.SetCookie(writer, &http.Cookie{Name: "session", Value: "abc"})
		writer.Header().Set("Content-Type", "image/jpeg")
		writer.WriteHeader(http.StatusCreated)
		writer.Write([]byte{0xFF, 0xD8})
	})
	event := Event{RawPath: "/api/v1/members/me/avatar", RawQueryString: "a=1", Cookies: []string{"first=1", "second=2"},
		Headers: map[string]string{"authorization": "FitHalo-ECDSA x", "content-type": "image/png"},
		Body:    base64.StdEncoding.EncodeToString([]byte{0x89, 0x50}), IsBase64Encoded: true}
	event.RequestContext.DomainName = "abc.lambda-url.us-east-1.on.aws"
	event.RequestContext.HTTP.Method = "PUT"
	event.RequestContext.HTTP.SourceIP = "203.0.113.9"

	response := Handle(context.Background(), handler, event)
	if seen.RequestURI != "/api/v1/members/me/avatar?a=1" || seen.Host != event.RequestContext.DomainName ||
		seen.RemoteAddr != "203.0.113.9:0" || seen.Header.Get("Authorization") != "FitHalo-ECDSA x" ||
		seen.Header.Get("X-Forwarded-Proto") != "https" || !strings.Contains(seen.Header.Get("Cookie"), "second=2") ||
		string(seenBody) != "\x89\x50" {
		t.Fatalf("the request was %+v with body %q", seen, seenBody)
	}
	body, _ := base64.StdEncoding.DecodeString(response.Body)
	if response.StatusCode != http.StatusCreated || response.Headers["Content-Type"] != "image/jpeg" ||
		len(response.Cookies) != 1 || string(body) != "\xFF\xD8" {
		t.Fatalf("the response was %+v", response)
	}
}

// fakeRuntime is Lambda's runtime API: it hands out queued events and records the answers.
type fakeRuntime struct {
	// mutex guards every field below it.
	mutex sync.Mutex

	// events is the payloads still to hand out, in order.
	events [][]byte

	// answers holds each answer's body by its path after the invocation prefix.
	answers map[string]string
}

// ServeHTTP hands out the next event, or 503 when none is left, and records anything posted.
func (f *fakeRuntime) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if strings.HasSuffix(request.URL.Path, "/next") {
		if len(f.events) == 0 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		id := "request-" + string(rune('a'+len(f.answers)))
		writer.Header().Set("Lambda-Runtime-Aws-Request-Id", id)
		writer.Header().Set("Lambda-Runtime-Deadline-Ms", "9999999999999")
		writer.Write(f.events[0])
		f.events = f.events[1:]
		return
	}
	body, _ := io.ReadAll(request.Body)
	f.answers[strings.TrimPrefix(request.URL.Path, runtimePrefix)] = string(body)
}

// TestRunAnswersRequestsAndTheSchedule serves a request and a scheduled sweep through the runtime API.
func TestRunAnswersRequestsAndTheSchedule(t *testing.T) {
	request := Event{RawPath: "/api/v1/board"}
	request.RequestContext.DomainName = "abc.lambda-url.us-east-1.on.aws"
	request.RequestContext.HTTP.Method = "GET"
	requestJSON, _ := json.Marshal(request)
	fake := &fakeRuntime{events: [][]byte{requestJSON, []byte(`{"source":"aws.events","detail-type":"Scheduled Event"}`)}, answers: map[string]string{}}
	server := httptest.NewServer(fake)
	defer server.Close()

	domains, swept := []string{}, 0
	runtime := &Runtime{
		API: strings.TrimPrefix(server.URL, "http://"),
		Handler: func(domain string) (http.Handler, error) {
			domains = append(domains, domain)
			return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.Write([]byte("board")) }), nil
		},
		Scheduled: func(context.Context) error { swept++; return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runtime.Run(ctx)

	if len(domains) != 1 || domains[0] != request.RequestContext.DomainName {
		t.Fatalf("the handler was built for %v", domains)
	}
	if swept != 1 {
		t.Fatalf("the schedule swept %d times", swept)
	}
	var answer Response
	json.Unmarshal([]byte(fake.answers["request-a/response"]), &answer)
	body, _ := base64.StdEncoding.DecodeString(answer.Body)
	if answer.StatusCode != http.StatusOK || string(body) != "board" {
		t.Fatalf("the request was answered with %s", fake.answers["request-a/response"])
	}
	if _, answered := fake.answers["request-b/response"]; !answered {
		t.Fatalf("the schedule was not answered: %v", fake.answers)
	}
}

// TestHandlerIsAskedUntilItSucceeds proves a failed Handler is asked again and a built one is kept.
func TestHandlerIsAskedUntilItSucceeds(t *testing.T) {
	request := Event{RawPath: "/api/v1/board"}
	request.RequestContext.DomainName = "abc.lambda-url.us-east-1.on.aws"
	request.RequestContext.HTTP.Method = "GET"
	requestJSON, _ := json.Marshal(request)
	fake := &fakeRuntime{events: [][]byte{requestJSON, requestJSON, requestJSON}, answers: map[string]string{}}
	server := httptest.NewServer(fake)
	defer server.Close()

	asked := 0
	runtime := &Runtime{
		API: strings.TrimPrefix(server.URL, "http://"),
		Handler: func(string) (http.Handler, error) {
			asked++
			if asked == 1 {
				return nil, errors.New("not yet")
			}
			return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.Write([]byte("board")) }), nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runtime.Run(ctx)

	if asked != 2 {
		t.Fatalf("got Handler asked %d times, want 2", asked)
	}
	_, failed := fake.answers["request-a/error"]
	_, second := fake.answers["request-b/response"]
	_, third := fake.answers["request-c/response"]
	if !failed || !second || !third {
		t.Fatalf("got answers %v, want an error then two responses", fake.answers)
	}
}

// TestHandleRefusesMalformedRequests answers 400 to a bad target or method instead of panicking.
func TestHandleRefusesMalformedRequests(t *testing.T) {
	served := false
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })
	for _, test := range []struct{ method, path, query string }{
		{"GET", "/%zz", ""},
		{"GET", "/a b", ""},
		{"GET", "/a\x00b", ""},
		{"GET", "/board", "a=1 HTTP/1.1\r\nHost: x"},
		{"GET", "board", ""},
		{"GET", "", ""},
		{"BAD METHOD", "/board", ""},
		{"PROPFIND", "/board", ""},
		{"", "/board", ""},
	} {
		event := Event{RawPath: test.path, RawQueryString: test.query}
		event.RequestContext.HTTP.Method = test.method
		response := Handle(context.Background(), handler, event)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("Handle(%q %q?%q) answered %d, want 400", test.method, test.path, test.query, response.StatusCode)
		}
	}
	if served {
		t.Fatal("a malformed request reached the handler")
	}
}

// TestRunSurvivesPanics proves a panicking handler is answered 500 and a panicking sweep with an error,
// and Run goes on to the next invocation.
func TestRunSurvivesPanics(t *testing.T) {
	request := Event{RawPath: "/api/v1/board"}
	request.RequestContext.DomainName = "abc.lambda-url.us-east-1.on.aws"
	request.RequestContext.HTTP.Method = "GET"
	requestJSON, _ := json.Marshal(request)
	schedule := []byte(`{"source":"aws.events","detail-type":"Scheduled Event"}`)
	fake := &fakeRuntime{events: [][]byte{requestJSON, schedule, requestJSON}, answers: map[string]string{}}
	server := httptest.NewServer(fake)
	defer server.Close()

	calls := 0
	runtime := &Runtime{
		API: strings.TrimPrefix(server.URL, "http://"),
		Handler: func(string) (http.Handler, error) {
			return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					panic("handler failed")
				}
				writer.Write([]byte("board"))
			}), nil
		},
		Scheduled: func(context.Context) error { panic("sweep failed") },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runtime.Run(ctx)

	var first, third Response
	json.Unmarshal([]byte(fake.answers["request-a/response"]), &first)
	json.Unmarshal([]byte(fake.answers["request-c/response"]), &third)
	_, sweepFailed := fake.answers["request-b/error"]
	if first.StatusCode != http.StatusInternalServerError || !sweepFailed || third.StatusCode != http.StatusOK {
		t.Fatalf("got answers %v, want a 500, a sweep error, then a 200", fake.answers)
	}
}
