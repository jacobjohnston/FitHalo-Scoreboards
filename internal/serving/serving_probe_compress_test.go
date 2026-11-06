package serving_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/serving"
)

// compressible is a 2 KB body that gzip shrinks.
var compressible = strings.Repeat("{\"k\":\"value\"},", 150)

// bodyHandler answers status with body as contentType, setting etag when given.
func bodyHandler(status int, contentType, etag, body string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			writer.Header().Set("Content-Type", contentType)
		}
		if etag != "" {
			writer.Header().Set("ETag", etag)
		}
		writer.WriteHeader(status)
		writer.Write([]byte(body))
	})
}

// compressed sends a request with acceptEncoding through Compress(handler) and returns the result.
func compressed(handler http.Handler, method, acceptEncoding string, header http.Header) *http.Response {
	request := httptest.NewRequest(method, "/", nil)
	if acceptEncoding != "-" {
		request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	for key, values := range header {
		request.Header[key] = values
	}
	recorder := httptest.NewRecorder()
	serving.Compress(handler).ServeHTTP(recorder, request)
	return recorder.Result()
}

// readBody returns the response body, gunzipped when Content-Encoding says so.
func readBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Content-Encoding") != "gzip" {
		return data
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the body is not gzip: %v", err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("the gzip body is damaged: %v", err)
	}
	return plain
}

// TestCompressThreshold gzips at 1 KB and not a byte under.
func TestCompressThreshold(t *testing.T) {
	for size, want := range map[int]bool{0: false, 1: false, 1023: false, 1024: true, 1025: true, 65536: true} {
		body := strings.Repeat("a", size)
		response := compressed(bodyHandler(200, "application/json", "", body), "GET", "gzip", nil)
		if got := response.Header.Get("Content-Encoding") == "gzip"; got != want {
			t.Errorf("a %d-byte body: gzipped %v, want %v", size, got, want)
		}
		if got := string(readBody(t, response)); got != body {
			t.Errorf("a %d-byte body came back as %d bytes", size, len(got))
		}
	}
}

// TestCompressAcceptEncoding reads the client's Accept-Encoding by RFC 9110.
func TestCompressAcceptEncoding(t *testing.T) {
	cases := []struct {
		// name names the case.
		name string

		// value is the Accept-Encoding header; "-" sends none.
		value string

		// want says whether gzip is expected.
		want bool
	}{
		{"gzip", "gzip", true},
		{"uppercase", "GZIP", true},
		{"q zero", "gzip;q=0", false},
		{"q zero spaced", "gzip; q=0", false},
		{"q zero decimal", "gzip;q=0.000", false},
		{"identity", "identity", false},
		{"star", "*", true},
		{"star q zero with gzip", "gzip, *;q=0", true},
		{"br then gzip", "br, gzip;q=0.5", true},
		{"tiny q", "gzip;q=0.001", true},
		{"missing", "-", false},
		{"empty", "", false},
		{"deflate only", "deflate", false},
		{"gzip in list", "deflate, gzip, br", true},
		{"gzipped is not gzip", "gzipped", false},
		{"spaces", "  gzip  ", true},
		{"q one", "gzip;q=1.0", true},
	}
	for _, c := range cases {
		response := compressed(bodyHandler(200, "application/json", "", compressible), "GET", c.value, nil)
		if got := response.Header.Get("Content-Encoding") == "gzip"; got != c.want {
			t.Errorf("%s: Accept-Encoding %q: gzipped %v, want %v", c.name, c.value, got, c.want)
		}
	}
}

// TestCompressStarAndQValues records the gzip decision for subtler Accept-Encoding forms.
func TestCompressStarAndQValues(t *testing.T) {
	// PROBE: "*;q=0" alone forbids every coding; x-gzip is gzip's alias a recipient SHOULD honour.
	for _, value := range []string{"*;q=0", "identity;q=0", "gzip;q=0.5, identity;q=1", "x-gzip"} {
		response := compressed(bodyHandler(200, "application/json", "", compressible), "GET", value, nil)
		t.Logf("Accept-Encoding %q: Content-Encoding %q", value, response.Header.Get("Content-Encoding"))
	}
}

// TestCompressExplicitGzipQZeroBeatsStar never gzips when gzip itself carries q=0, whatever * says.
func TestCompressExplicitGzipQZeroBeatsStar(t *testing.T) {
	// RFC 9110 8.4.1: a listed coding "is acceptable unless it is accompanied by a qvalue of 0".
	if response := compressed(bodyHandler(200, "application/json", "", compressible), "GET", "gzip;q=0, *", nil); response.Header.Get("Content-Encoding") == "gzip" {
		t.Error("gzip;q=0, * gzipped although gzip is forbidden")
	}
}

// TestCompressHeaders sets Content-Encoding, drops Content-Length, and adds Vary.
func TestCompressHeaders(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Length", strconv.Itoa(len(compressible)))
		writer.Write([]byte(compressible))
	})
	response := compressed(handler, "GET", "gzip", nil)
	if response.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("not gzipped")
	}
	raw, _ := io.ReadAll(response.Body)
	if length := response.Header.Get("Content-Length"); length != "" && length != strconv.Itoa(len(raw)) {
		t.Errorf("Content-Length %s but the gzipped body is %d bytes (a stale length from the handler)", length, len(raw))
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Vary")), "accept-encoding") {
		t.Errorf("Vary is %q on a gzipped response", response.Header.Get("Vary"))
	}
	if len(raw) >= len(compressible) {
		t.Errorf("gzip made the body %d bytes from %d", len(raw), len(compressible))
	}
}

// TestCompressVaryWhenNotGzipped adds Vary even when the client gets identity, so caches split.
func TestCompressVaryWhenNotGzipped(t *testing.T) {
	response := compressed(bodyHandler(200, "application/json", "", compressible), "GET", "-", nil)
	if !strings.Contains(strings.ToLower(response.Header.Get("Vary")), "accept-encoding") {
		t.Errorf("Vary is %q on an uncompressed large JSON response", response.Header.Get("Vary"))
	}
	small := compressed(bodyHandler(200, "application/json", "", "tiny"), "GET", "gzip", nil)
	// PROBE: a body under the threshold is the same for every client, so Vary is optional there.
	t.Logf("Vary on a tiny response: %q", small.Header.Get("Vary"))
}

// TestCompressKeepsExistingVary does not lose a Vary the handler set.
func TestCompressKeepsExistingVary(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Vary", "Accept-Language")
		writer.Write([]byte(compressible))
	})
	response := compressed(handler, "GET", "gzip", nil)
	vary := strings.ToLower(strings.Join(response.Header.Values("Vary"), ","))
	if !strings.Contains(vary, "accept-language") || !strings.Contains(vary, "accept-encoding") {
		t.Errorf("Vary is %q, want both Accept-Language and Accept-Encoding", response.Header.Values("Vary"))
	}
}

// TestCompressWeakensStrongETag turns "abc" into W/"abc" only when gzipping.
func TestCompressWeakensStrongETag(t *testing.T) {
	gzipped := compressed(bodyHandler(200, "application/json", `"abc"`, compressible), "GET", "gzip", nil)
	if gzipped.Header.Get("ETag") != `W/"abc"` {
		t.Errorf("gzipped ETag is %q, want W/\"abc\"", gzipped.Header.Get("ETag"))
	}
	plain := compressed(bodyHandler(200, "application/json", `"abc"`, compressible), "GET", "-", nil)
	if plain.Header.Get("ETag") != `"abc"` {
		t.Errorf("uncompressed ETag is %q, want \"abc\"", plain.Header.Get("ETag"))
	}
	small := compressed(bodyHandler(200, "application/json", `"abc"`, "tiny"), "GET", "gzip", nil)
	if small.Header.Get("ETag") != `"abc"` {
		t.Errorf("a small body's ETag is %q, want the strong \"abc\"", small.Header.Get("ETag"))
	}
	weak := compressed(bodyHandler(200, "application/json", `W/"abc"`, compressible), "GET", "gzip", nil)
	if weak.Header.Get("ETag") != `W/"abc"` {
		t.Errorf("an already weak ETag became %q", weak.Header.Get("ETag"))
	}
}

// weakComparing answers 304 when If-None-Match matches its ETag by RFC 9110's weak comparison.
func weakComparing(etag string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("ETag", etag)
		for _, candidate := range strings.Split(request.Header.Get("If-None-Match"), ",") {
			if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == strings.TrimPrefix(etag, "W/") {
				writer.WriteHeader(http.StatusNotModified)
				return
			}
		}
		writer.Write([]byte(compressible))
	})
}

// strictComparing answers 304 only when If-None-Match equals its ETag byte for byte.
func strictComparing(etag string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("ETag", etag)
		if request.Header.Get("If-None-Match") == etag {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		writer.Write([]byte(compressible))
	})
}

// TestCompressIfNoneMatchEitherForm gets 304 for the weak and strong form, gzipped or not.
func TestCompressIfNoneMatchEitherForm(t *testing.T) {
	for _, ifNoneMatch := range []string{`"abc"`, `W/"abc"`} {
		for _, accept := range []string{"gzip", "-"} {
			response := compressed(weakComparing(`"abc"`), "GET", accept, http.Header{"If-None-Match": {ifNoneMatch}})
			if response.StatusCode != http.StatusNotModified {
				t.Errorf("If-None-Match %s with Accept-Encoding %q answered %d, want 304", ifNoneMatch, accept, response.StatusCode)
			}
			if body, _ := io.ReadAll(response.Body); len(body) != 0 {
				t.Errorf("If-None-Match %s with Accept-Encoding %q: a 304 carried %d bytes", ifNoneMatch, accept, len(body))
			}
			if response.Header.Get("Content-Encoding") != "" {
				t.Errorf("a 304 carries Content-Encoding %q", response.Header.Get("Content-Encoding"))
			}
		}
	}
}

// TestCompressIfNoneMatchWithStrictHandler records whether Compress itself normalises If-None-Match.
func TestCompressIfNoneMatchWithStrictHandler(t *testing.T) {
	// PROBE: the contract says If-None-Match accepts either form; this shows whether Compress makes that
	// true for a handler that compares strictly, or whether every handler must compare weakly.
	for _, ifNoneMatch := range []string{`"abc"`, `W/"abc"`} {
		for _, accept := range []string{"gzip", "-"} {
			response := compressed(strictComparing(`"abc"`), "GET", accept, http.Header{"If-None-Match": {ifNoneMatch}})
			t.Logf("strict handler, If-None-Match %s, Accept-Encoding %q: %d", ifNoneMatch, accept, response.StatusCode)
		}
	}
}

// TestCompress304ETagForm records the ETag form on a 304 to a gzip client.
func TestCompress304ETagForm(t *testing.T) {
	// PROBE: RFC 9110 says a 304 carries the ETag the 200 would have carried, which is the weak form here.
	response := compressed(weakComparing(`"abc"`), "GET", "gzip", http.Header{"If-None-Match": {`"abc"`}})
	t.Logf("304 to a gzip client has ETag %q", response.Header.Get("ETag"))
}

// TestCompressContentTypes gzips text and JSON, leaves already compressed media alone.
func TestCompressContentTypes(t *testing.T) {
	cases := map[string]bool{
		"application/json":                true,
		"application/json; charset=utf-8": true,
		"application/problem+json":        true,
		// HTML is left out on purpose: a portal page holds a CSRF token beside text a member chose, which a
		// compression-length attack needs together.
		"text/html":                false,
		"text/html; charset=utf-8": false,
		"text/plain":               true,
		"text/css":                 true,
		"text/javascript":          true,
		"image/svg+xml":            true,
		"image/jpeg":               false,
		"image/png":                false,
		"application/gzip":         false,
		"application/zip":          false,
		"font/woff2":               false,
		"application/octet-stream": false,
		"video/mp4":                false,
	}
	for contentType, want := range cases {
		response := compressed(bodyHandler(200, contentType, "", compressible), "GET", "gzip", nil)
		if got := response.Header.Get("Content-Encoding") == "gzip"; got != want {
			t.Errorf("%s: gzipped %v, want %v", contentType, got, want)
		}
		if string(readBody(t, response)) != compressible {
			t.Errorf("%s: the body changed", contentType)
		}
	}
}

// TestCompressNoContentType records what happens when the handler sets no Content-Type.
func TestCompressNoContentType(t *testing.T) {
	// PROBE: without a type, net/http sniffs; JSON sniffs as text/plain, which compresses.
	response := compressed(bodyHandler(200, "", "", compressible), "GET", "gzip", nil)
	t.Logf("no Content-Type: Content-Encoding %q, Content-Type %q", response.Header.Get("Content-Encoding"), response.Header.Get("Content-Type"))
	if string(readBody(t, response)) != compressible {
		t.Error("the body changed")
	}
}

// TestCompressOnlySuccessfulResponses leaves a large problem body alone, per the contract's "successful".
func TestCompressOnlySuccessfulResponses(t *testing.T) {
	for _, status := range []int{200, 201} {
		response := compressed(bodyHandler(status, "application/json", "", compressible), "GET", "gzip", nil)
		if response.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("a %d was not gzipped", status)
		}
	}
	// PROBE: "successful responses" in the doc; the openapi says "a successful body is 1 KB or more".
	// Whether a 2 KB 404 problem is gzipped is harmless either way; record which.
	for _, status := range []int{400, 404, 500} {
		response := compressed(bodyHandler(status, "application/problem+json", "", compressible), "GET", "gzip", nil)
		t.Logf("a %d problem of 2 KB: Content-Encoding %q", status, response.Header.Get("Content-Encoding"))
		if string(readBody(t, response)) != compressible {
			t.Errorf("a %d body changed", status)
		}
	}
}

// TestCompressNoBodyStatuses leaves 204 and 304 without Content-Encoding or body.
func TestCompressNoBodyStatuses(t *testing.T) {
	for _, status := range []int{204, 304} {
		handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(status)
		})
		response := compressed(handler, "GET", "gzip", nil)
		if response.StatusCode != status {
			t.Errorf("a %d became %d", status, response.StatusCode)
		}
		if response.Header.Get("Content-Encoding") != "" {
			t.Errorf("a %d carries Content-Encoding %q", status, response.Header.Get("Content-Encoding"))
		}
		if body, _ := io.ReadAll(response.Body); len(body) != 0 {
			t.Errorf("a %d carries %d body bytes", status, len(body))
		}
	}
}

// TestCompressLeavesEncodedResponsesAlone does not double-encode a response already encoded.
func TestCompressLeavesEncodedResponsesAlone(t *testing.T) {
	var already bytes.Buffer
	writer := gzip.NewWriter(&already)
	writer.Write([]byte(compressible))
	writer.Close()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Encoding", "gzip")
		writer.Write(already.Bytes())
	})
	response := compressed(handler, "GET", "gzip", nil)
	if string(readBody(t, response)) != compressible {
		t.Error("an already gzipped body was gzipped again")
	}
	brotli := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Encoding", "br")
		writer.Write([]byte(compressible))
	})
	response = compressed(brotli, "GET", "gzip", nil)
	if response.Header.Get("Content-Encoding") != "br" {
		t.Errorf("a br response became %q", response.Header.Get("Content-Encoding"))
	}
	if raw, _ := io.ReadAll(response.Body); string(raw) != compressible {
		t.Error("a br response's bytes were changed")
	}
}

// TestCompressImplicitStatus gzips when the handler never calls WriteHeader.
func TestCompressImplicitStatus(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.Write([]byte(compressible))
	})
	response := compressed(handler, "GET", "gzip", nil)
	if response.StatusCode != 200 || response.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("implicit 200: status %d, Content-Encoding %q", response.StatusCode, response.Header.Get("Content-Encoding"))
	}
	if string(readBody(t, response)) != compressible {
		t.Error("the body changed")
	}
}

// TestCompressManySmallWrites decides the threshold over the whole body, not the first write.
func TestCompressManySmallWrites(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		for index := 0; index < 2000; index++ {
			writer.Write([]byte{'x'})
		}
	})
	response := compressed(handler, "GET", "gzip", nil)
	if response.Header.Get("Content-Encoding") != "gzip" {
		t.Error("2000 single-byte writes were not gzipped")
	}
	if got := readBody(t, response); len(got) != 2000 || strings.Trim(string(got), "x") != "" {
		t.Errorf("the body came back as %d bytes", len(got))
	}
	short := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		for index := 0; index < 1023; index++ {
			writer.Write([]byte{'x'})
		}
	})
	response = compressed(short, "GET", "gzip", nil)
	if response.Header.Get("Content-Encoding") == "gzip" {
		t.Error("1023 single-byte writes were gzipped")
	}
	if got := readBody(t, response); len(got) != 1023 {
		t.Errorf("the short body came back as %d bytes", len(got))
	}
}

// TestCompressStreamingWithFlush keeps a flushed response consistent, whichever coding it chose.
func TestCompressStreamingWithFlush(t *testing.T) {
	flusherSeen := false
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.Write([]byte("first chunk, "))
		if flusher, ok := writer.(http.Flusher); ok {
			flusherSeen = true
			flusher.Flush()
		}
		writer.Write([]byte(compressible))
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		writer.Write([]byte(", last chunk"))
	})
	server := httptest.NewServer(serving.Compress(handler))
	defer server.Close()
	request, _ := http.NewRequest("GET", server.URL, nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := readBody(t, response)
	want := "first chunk, " + compressible + ", last chunk"
	if string(body) != want {
		t.Errorf("a flushed body came back as %d bytes, want %d", len(body), len(want))
	}
	// PROBE: a handler that streams needs Flush to reach the client; record whether the wrapper offers it.
	t.Logf("the wrapped writer implements http.Flusher: %v; Content-Encoding %q", flusherSeen, response.Header.Get("Content-Encoding"))
}

// TestCompressHEAD answers a HEAD without a body, consistently with GET's headers.
func TestCompressHEAD(t *testing.T) {
	server := httptest.NewServer(serving.Compress(bodyHandler(200, "application/json", `"abc"`, compressible)))
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	for _, method := range []string{"GET", "HEAD"} {
		request, _ := http.NewRequest(method, server.URL, nil)
		request.Header.Set("Accept-Encoding", "gzip")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Logf("%s: status %d, Content-Encoding %q, ETag %q, Content-Length %q, %d body bytes", method, response.StatusCode,
			response.Header.Get("Content-Encoding"), response.Header.Get("ETag"), response.Header.Get("Content-Length"), len(body))
		if method == "HEAD" && len(body) != 0 {
			t.Error("a HEAD answer carried a body")
		}
		if response.StatusCode != 200 {
			t.Errorf("%s answered %d", method, response.StatusCode)
		}
	}
}

// TestCompressRoundTripsRandomBodies decompresses to the exact original bytes for random bodies.
func TestCompressRoundTripsRandomBodies(t *testing.T) {
	random := rand.New(rand.NewSource(42))
	types := []string{"application/json", "text/plain", "image/jpeg", "application/problem+json"}
	for round := 0; round < 200; round++ {
		size := random.Intn(5000)
		if round%10 == 0 {
			size = 1024 + random.Intn(3) - 1
		}
		body := make([]byte, size)
		if random.Intn(2) == 0 {
			random.Read(body)
		} else {
			for index := range body {
				body[index] = byte('a' + random.Intn(4))
			}
		}
		contentType := types[random.Intn(len(types))]
		accept := []string{"gzip", "-", "identity"}[random.Intn(3)]
		writes := 1 + random.Intn(4)
		handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", contentType)
			for index := 0; index < writes; index++ {
				from, to := index*len(body)/writes, (index+1)*len(body)/writes
				writer.Write(body[from:to])
			}
		})
		response := compressed(handler, "GET", accept, nil)
		got := readBody(t, response)
		if !bytes.Equal(got, body) {
			t.Fatalf("round %d (%d bytes, %s, Accept-Encoding %q, %d writes): the body changed", round, size, contentType, accept, writes)
		}
		if gz := response.Header.Get("Content-Encoding") == "gzip"; gz && (accept == "-" || accept == "identity" || size < 1024 || contentType == "image/jpeg") {
			t.Fatalf("round %d (%d bytes, %s, Accept-Encoding %q): gzipped against the rules", round, size, contentType, accept)
		}
		if length := response.Header.Get("Content-Length"); length != "" {
			raw := response.ContentLength
			if raw >= 0 && response.Header.Get("Content-Encoding") == "" && int(raw) != size {
				t.Fatalf("round %d: Content-Length %d for a %d-byte identity body", round, raw, size)
			}
		}
	}
}

// TestCompressConcurrent serves many clients at once through one Compress handler.
func TestCompressConcurrent(t *testing.T) {
	handler := serving.Compress(bodyHandler(200, "application/json", `"v1"`, compressible))
	var group sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			accept := "gzip"
			if worker%3 == 0 {
				accept = "-"
			}
			for round := 0; round < 20; round++ {
				request := httptest.NewRequest("GET", "/", nil)
				if accept != "-" {
					request.Header.Set("Accept-Encoding", accept)
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if string(readBody(t, recorder.Result())) != compressible {
					t.Errorf("worker %d: the body changed", worker)
					return
				}
			}
		}(worker)
	}
	group.Wait()
}

// TestCompressLargeBody round-trips a 5 MB body.
func TestCompressLargeBody(t *testing.T) {
	large := bytes.Repeat([]byte("0123456789abcdef"), 5*1024*1024/16)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Write(large)
	})
	response := compressed(handler, "GET", "gzip", nil)
	if response.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("a 5 MB body was not gzipped")
	}
	if !bytes.Equal(readBody(t, response), large) {
		t.Fatal("the 5 MB body changed")
	}
}

// TestCompressHandlerSetsContentLengthShortOfThreshold trusts the bytes written, not the header.
func TestCompressHandlerSetsContentLengthShortOfThreshold(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Length", "5000")
		writer.Write([]byte("short"))
	})
	response := compressed(handler, "GET", "gzip", nil)
	body, _ := io.ReadAll(response.Body)
	// PROBE: a handler lying about Content-Length is its own bug; this only checks nothing is corrupted.
	t.Logf("Content-Length 5000 with a 5-byte write: Content-Encoding %q, Content-Length %q, %d bytes", response.Header.Get("Content-Encoding"), response.Header.Get("Content-Length"), len(body))
	if response.Header.Get("Content-Encoding") == "gzip" {
		if string(readBody(t, response)) != "short" {
			t.Error("the short body is damaged")
		}
	}
}
