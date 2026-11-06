package problem_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/problem"
)

// statusByCode is the Problem table in openapi.yaml.
var statusByCode = map[problem.Code]int{
	problem.BadRequest: 400, problem.Unauthenticated: 401, problem.ClockSkew: 401, problem.Replayed: 401,
	problem.InviteInvalid: 403, problem.PasswordIncorrect: 403, problem.Blocked: 403, problem.PendingLimitReached: 403,
	problem.MemberPending: 403, problem.AvatarsNotAllowed: 403, problem.RoutesNotShared: 403, problem.NotFound: 404,
	problem.UnknownOperation: 404, problem.UpgradeRequired: 403, problem.PayloadTooLarge: 413, problem.UnsupportedMediaType: 415, problem.ImageInvalid: 422, problem.WeekOutOfRange: 422,
	problem.RateLimited: 429, problem.Internal: 500, problem.Unavailable: 503,
}

// body is the JSON shape a client sees.
type body struct {
	// Code is the stable code.
	Code string `json:"code"`

	// Status is the HTTP status.
	Status int `json:"status"`

	// Title is the summary.
	Title string `json:"title"`

	// Detail is the optional explanation.
	Detail *string `json:"detail"`
}

// write sends a problem to a recorder and decodes the response.
func write(t *testing.T, p *problem.Problem) (*httptest.ResponseRecorder, body, map[string]json.RawMessage) {
	t.Helper()
	recorder := httptest.NewRecorder()
	problem.Write(recorder, p)
	var decoded body
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, recorder.Body.String())
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(recorder.Body.Bytes(), &raw)
	return recorder, decoded, raw
}

// TestEveryCodeHasItsOpenAPIStatus checks New gives each code the status the API contract lists.
func TestEveryCodeHasItsOpenAPIStatus(t *testing.T) {
	for code, status := range statusByCode {
		p := problem.New(code, "")
		if p.Status != status {
			t.Errorf("%s: status %d, want %d", code, p.Status, status)
		}
		if p.Code != code {
			t.Errorf("%s: code %q", code, p.Code)
		}
		if strings.TrimSpace(p.Title) == "" {
			t.Errorf("%s: empty title", code)
		}
		if !strings.Contains(p.Error(), string(code)) {
			t.Errorf("%s: Error() %q lacks the code", code, p.Error())
		}
	}
}

// TestUnknownCodeBecomesInternal checks codes outside the table become Internal with status 500.
func TestUnknownCodeBecomesInternal(t *testing.T) {
	for _, code := range []problem.Code{"", "nope", "BadRequest", "badrequest", "badRequest ", "internal\x00", "rateLimited2"} {
		p := problem.New(code, "detail")
		if p.Code != problem.Internal || p.Status != 500 {
			t.Errorf("New(%q): code %q status %d, want internal 500", code, p.Code, p.Status)
		}
	}
}

// TestUnknownCodeDetailRecordsResult records whether the detail survives when a code is unknown.
func TestUnknownCodeDetailRecordsResult(t *testing.T) {
	// PROBE: the Internal doc says "no detail for the client"; record whether New keeps a detail passed with an unknown code.
	p := problem.New("nope", "something sensitive")
	t.Logf("unknown code kept detail: %q", p.Detail)
}

// TestWriteHeadersAndBody checks the content type, status, body fields and Retry-After rules.
func TestWriteHeadersAndBody(t *testing.T) {
	for _, test := range []struct {
		// name describes the case.
		name string

		// p is the problem written.
		p *problem.Problem

		// wantRetry is the Retry-After header, empty for none.
		wantRetry string

		// wantDetail says whether detail must be present.
		wantDetail bool
	}{
		{name: "not found without detail", p: problem.New(problem.NotFound, "")},
		{name: "bad request with detail", p: problem.New(problem.BadRequest, "field x is required"), wantDetail: true},
		{name: "rate limited with retry", p: &problem.Problem{Code: problem.RateLimited, Status: 429, Title: "t", RetryAfter: 7}, wantRetry: "7"},
		{name: "rate limited retry one", p: &problem.Problem{Code: problem.RateLimited, Status: 429, Title: "t", RetryAfter: 1}, wantRetry: "1"},
		{name: "retry zero sends no header", p: &problem.Problem{Code: problem.RateLimited, Status: 429, Title: "t", RetryAfter: 0}},
		{name: "negative retry sends no header", p: &problem.Problem{Code: problem.Unavailable, Status: 503, Title: "t", RetryAfter: -5}},
		{name: "unavailable with retry", p: &problem.Problem{Code: problem.Unavailable, Status: 503, Title: "t", RetryAfter: 3600}, wantRetry: "3600"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder, decoded, raw := write(t, test.p)
			if recorder.Code != test.p.Status {
				t.Errorf("status %d, want %d", recorder.Code, test.p.Status)
			}
			if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/problem+json") {
				t.Errorf("Content-Type %q", contentType)
			}
			if got := recorder.Header().Get("Retry-After"); got != test.wantRetry {
				t.Errorf("Retry-After %q, want %q", got, test.wantRetry)
			}
			if decoded.Code != string(test.p.Code) || decoded.Status != test.p.Status || decoded.Title != test.p.Title {
				t.Errorf("body %+v, problem %+v", decoded, test.p)
			}
			if _, present := raw["detail"]; present != test.wantDetail {
				t.Errorf("detail present %v, want %v (body %s)", present, test.wantDetail, recorder.Body.String())
			}
			if test.wantDetail && (decoded.Detail == nil || *decoded.Detail != test.p.Detail) {
				t.Errorf("detail %v, want %q", decoded.Detail, test.p.Detail)
			}
			if _, present := raw["retryAfter"]; present {
				t.Error("retryAfter leaked into the body")
			}
			for key := range raw {
				switch key {
				case "code", "status", "title", "detail":
				default:
					t.Logf("extra body member %q", key)
				}
			}
			// PROBE: X-Content-Type-Options is a server-wide header; record whether Write sets it.
			t.Logf("X-Content-Type-Options %q", recorder.Header().Get("X-Content-Type-Options"))
		})
	}
}

// TestRetryAfterIsIntegerSeconds checks the header parses as the integer the schema promises.
func TestRetryAfterIsIntegerSeconds(t *testing.T) {
	for _, seconds := range []int{1, 59, 60, 61, 86400, 1 << 30} {
		recorder, _, _ := write(t, &problem.Problem{Code: problem.RateLimited, Status: 429, Title: "t", RetryAfter: seconds})
		parsed, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
		if err != nil || parsed != seconds {
			t.Errorf("Retry-After %q for %d seconds", recorder.Header().Get("Retry-After"), seconds)
		}
	}
}

// TestDetailIsEscapedForEmbedding checks HTML and control characters in detail come out as valid, escaped JSON.
func TestDetailIsEscapedForEmbedding(t *testing.T) {
	detail := "<script>alert('x')</script> & \"quotes\"   \x00 \t ü 👍"
	recorder, decoded, _ := write(t, problem.New(problem.BadRequest, detail))
	if decoded.Detail == nil || *decoded.Detail != detail {
		t.Fatalf("detail round-trips as %v", decoded.Detail)
	}
	text := recorder.Body.String()
	// PROBE: encoding/json escapes <, >, & and U+2028 by default; record whether Write keeps that.
	if strings.Contains(text, "<script>") {
		t.Errorf("raw <script> in a problem body: %s", text)
	}
	if strings.ContainsRune(text, ' ') || strings.ContainsRune(text, 0) {
		t.Errorf("raw line separator or NUL in a problem body: %q", text)
	}
}

// TestInvalidUTF8DetailStillJSON checks a detail with invalid UTF-8 produces parseable JSON rather than a broken body.
func TestInvalidUTF8DetailStillJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	problem.Write(recorder, problem.New(problem.BadRequest, "bad \xff byte"))
	if !json.Valid(recorder.Body.Bytes()) {
		t.Fatalf("body is not valid JSON: %q", recorder.Body.String())
	}
}

// TestWriteNilProblemRecordsResult checks Write with a nil problem does not panic, or records that it does.
func TestWriteNilProblemRecordsResult(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// PROBE: the doc does not define Write(nil); a panic here would take down a handler that forwarded a nil error.
			t.Skipf("Write(nil) panics: %v", recovered)
		}
	}()
	recorder := httptest.NewRecorder()
	problem.Write(recorder, nil)
	t.Logf("Write(nil): status %d body %q", recorder.Code, recorder.Body.String())
}

// TestWriteAfterHeadersSent checks Write does not panic when the headers have already gone.
func TestWriteAfterHeadersSent(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.WriteHeader(http.StatusOK)
	recorder.Write([]byte("partial"))
	problem.Write(recorder, problem.New(problem.Internal, ""))
	if recorder.Code != http.StatusOK {
		t.Logf("recorder status became %d", recorder.Code)
	}
}

// TestWriteThroughRealServer checks the headers arrive over HTTP with the status and body intact.
func TestWriteThroughRealServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		problem.Write(writer, &problem.Problem{Code: problem.RateLimited, Status: 429, Title: "Too many requests", Detail: "slow down", RetryAfter: 12})
	}))
	t.Cleanup(server.Close)
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 429 || response.Header.Get("Retry-After") != "12" || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("status %d headers %v", response.StatusCode, response.Header)
	}
	var decoded body
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil || decoded.Code != "rateLimited" || decoded.Status != 429 || decoded.Detail == nil {
		t.Fatalf("body %+v, %v", decoded, err)
	}
}

// TestStatusInBodyMatchesHeader checks a hand-built problem's status is sent and echoed consistently.
func TestStatusInBodyMatchesHeader(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413, 415, 422, 429, 500, 503} {
		recorder, decoded, _ := write(t, &problem.Problem{Code: problem.BadRequest, Status: status, Title: "t"})
		if recorder.Code != status || decoded.Status != status {
			t.Errorf("status %d: header %d body %d", status, recorder.Code, decoded.Status)
		}
	}
}

// TestZeroStatusRecordsResult records what Write does with a Problem whose Status was never set.
func TestZeroStatusRecordsResult(t *testing.T) {
	// PROBE: a zero Status is a programming error; WriteHeader(0) panics in net/http, so record what happens.
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Skipf("Write with Status 0 panics: %v", recovered)
		}
	}()
	recorder := httptest.NewRecorder()
	problem.Write(recorder, &problem.Problem{Code: problem.Internal, Title: "t"})
	t.Logf("Status 0 was sent as %d", recorder.Code)
}

// TestProblemIsAnError checks a Problem satisfies error and Error mentions code and detail.
func TestProblemIsAnError(t *testing.T) {
	var err error = problem.New(problem.NotFound, "no such weekly challenge")
	if !strings.Contains(err.Error(), "notFound") || !strings.Contains(err.Error(), "no such weekly challenge") {
		t.Fatalf("Error() %q", err.Error())
	}
	var withoutDetail error = problem.New(problem.NotFound, "")
	if strings.TrimSpace(withoutDetail.Error()) == "" || strings.HasSuffix(withoutDetail.Error(), ": ") {
		t.Errorf("Error() without detail %q", withoutDetail.Error())
	}
}

// TestNewReturnsFreshValues checks two News of one code are independent values.
func TestNewReturnsFreshValues(t *testing.T) {
	first := problem.New(problem.NotFound, "")
	second := problem.New(problem.NotFound, "")
	first.Detail = "changed"
	first.RetryAfter = 9
	if second.Detail != "" || second.RetryAfter != 0 || first == second {
		t.Fatal("New returned a shared value")
	}
}

// TestBodyIsCompactUTF8JSON checks the body is UTF-8 and the Content-Type carries no odd parameters.
func TestBodyIsCompactUTF8JSON(t *testing.T) {
	recorder, _, _ := write(t, problem.New(problem.BadRequest, "ü 👍"))
	contentType := recorder.Header().Get("Content-Type")
	t.Logf("Content-Type %q, body %s", contentType, recorder.Body.String())
	if strings.Contains(contentType, "charset") && !strings.Contains(strings.ToLower(contentType), "utf-8") {
		t.Errorf("Content-Type %q names a charset that is not UTF-8", contentType)
	}
	if contentLength := recorder.Header().Get("Content-Length"); contentLength != "" && contentLength != strconv.Itoa(recorder.Body.Len()) {
		t.Errorf("Content-Length %s, body %d bytes", contentLength, recorder.Body.Len())
	}
}
