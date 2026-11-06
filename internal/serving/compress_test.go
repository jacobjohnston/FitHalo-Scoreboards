package serving

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// largeJSON is a JSON body well past minimumCompressedBytes.
var largeJSON = "[" + strings.Repeat(`{"displayName":"Avery","progress":0.5},`, 100) + "{}]"

// serveCompressed answers one request through Compress with a handler writing body as contentType.
func serveCompressed(t *testing.T, acceptEncoding, contentType string, status int, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := Compress(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", contentType)
		writer.Header().Set("ETag", `"abc"`)
		writer.WriteHeader(status)
		io.WriteString(writer, body)
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/board", nil)
	if acceptEncoding != "" {
		request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestCompressGzipsLargeJSON proves a large JSON body is gzipped, with a weak ETag and the exact body inside.
func TestCompressGzipsLargeJSON(t *testing.T) {
	response := serveCompressed(t, "br, gzip;q=0.8", "application/json", http.StatusOK, largeJSON)
	if response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("ETag") != `W/"abc"` ||
		response.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("got headers %v, want gzip, a weak ETag and Vary", response.Header())
	}
	if response.Body.Len() >= len(largeJSON) {
		t.Fatalf("got %d bytes sent, want fewer than %d", response.Body.Len(), len(largeJSON))
	}
	reader, err := gzip.NewReader(bytes.NewReader(response.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != largeJSON {
		t.Fatalf("got %d bytes after gunzip, %v; want the %d written", len(body), err, len(largeJSON))
	}
}

// TestCompressLeavesOtherResponsesAlone proves only an accepted, successful, compressible, large body
// is gzipped.
func TestCompressLeavesOtherResponsesAlone(t *testing.T) {
	for _, test := range []struct {
		name, acceptEncoding, contentType string
		status                            int
		body                              string
	}{
		{"not accepted", "", "application/json", http.StatusOK, largeJSON},
		{"refused by q", "gzip;q=0, deflate", "application/json", http.StatusOK, largeJSON},
		{"HTML", "gzip", "text/html; charset=utf-8", http.StatusOK, largeJSON},
		{"image", "gzip", "image/jpeg", http.StatusOK, largeJSON},
		{"small", "gzip", "application/json", http.StatusOK, `{"ok":true}`},
		{"not 200", "gzip", "application/problem+json", http.StatusBadRequest, largeJSON},
	} {
		response := serveCompressed(t, test.acceptEncoding, test.contentType, test.status, test.body)
		if response.Header().Get("Content-Encoding") != "" || response.Body.String() != test.body ||
			response.Code != test.status || response.Header().Get("ETag") != `"abc"` {
			t.Errorf("%s: got %d with headers %v, want the response untouched", test.name, response.Code, response.Header())
		}
	}
}

// TestAcceptsGzip reads Accept-Encoding the way RFC 9110 describes.
func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"gzip": true, "GZIP": true, "deflate, gzip": true, "*": true, "gzip;q=0.5": true,
		"": false, "br": false, "gzip;q=0": false, "gzip;q=0.0": false, "*;q=0": false, "gzip;q=x": false,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}
