package serving

import (
	"bytes"
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// minimumCompressedBytes is the smallest body worth compressing, below which gzip's own framing wins.
const minimumCompressedBytes = 1024

// compressibleTypes are the media types Compress gzips. HTML is left out, since a portal page holds a
// CSRF token beside text a member chose, which is what a compression-length attack needs.
var compressibleTypes = map[string]bool{
	"application/json":         true,
	"application/problem+json": true,
	"application/javascript":   true,
	"image/svg+xml":            true,
	"text/css":                 true,
	"text/javascript":          true,
	"text/plain":               true,
}

// gzipWriters holds gzip writers, which are costly to make.
var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// Compress gzips next's successful responses for a client that accepts gzip, when the type compresses
// and the body is large enough to gain. A strong ETag becomes weak, since the bytes sent differ.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodHead || !acceptsGzip(request.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(&varyingResponse{ResponseWriter: writer}, request)
			return
		}
		buffered := &bufferedResponse{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(buffered, request)
		body := buffered.body.Bytes()
		header := writer.Header()
		addVary(header)
		if isSuccess(buffered.status) && header.Get("Content-Encoding") == "" &&
			isCompressible(header.Get("Content-Type")) && len(body) >= minimumCompressedBytes {
			if compressed := gzipped(body); len(compressed) < len(body) {
				body = compressed
				header.Set("Content-Encoding", "gzip")
				header.Set("Content-Length", strconv.Itoa(len(body)))
				header.Del("Accept-Ranges")
				if etag := header.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
					header.Set("ETag", "W/"+etag)
				}
			}
		}
		writer.WriteHeader(buffered.status)
		writer.Write(body)
	})
}

// isSuccess reports whether a status is a 2xx that carries a body.
func isSuccess(status int) bool {
	return status >= 200 && status < 300 && status != http.StatusNoContent && status != http.StatusResetContent
}

// addVary adds Accept-Encoding to the response's Vary unless it is already there, keeping what the handler set.
func addVary(header http.Header) {
	for _, value := range header.Values("Vary") {
		for _, entry := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(entry), "Accept-Encoding") || strings.TrimSpace(entry) == "*" {
				return
			}
		}
	}
	header.Add("Vary", "Accept-Encoding")
}

// varyingResponse adds Accept-Encoding to Vary as the handler's headers go out, after the handler set its own.
type varyingResponse struct {
	// ResponseWriter is the client's writer.
	http.ResponseWriter

	// wroteHeader reports whether the headers have gone out.
	wroteHeader bool
}

// WriteHeader fixes Vary and sends the status.
func (v *varyingResponse) WriteHeader(status int) {
	if !v.wroteHeader {
		v.wroteHeader = true
		addVary(v.Header())
	}
	v.ResponseWriter.WriteHeader(status)
}

// Write sends part of the body, fixing Vary first when the handler never wrote a status.
func (v *varyingResponse) Write(data []byte) (int, error) {
	if !v.wroteHeader {
		v.WriteHeader(http.StatusOK)
	}
	return v.ResponseWriter.Write(data)
}

// bufferedResponse holds a response's status and body until the handler finishes.
type bufferedResponse struct {
	// ResponseWriter is the client's writer, whose headers the handler sets directly.
	http.ResponseWriter

	// status is the status the handler wrote, or 200 when it wrote none.
	status int

	// wroteHeader reports whether the handler has written its status.
	wroteHeader bool

	// body is everything the handler wrote.
	body bytes.Buffer
}

// WriteHeader records the status, keeping the first as net/http does.
func (b *bufferedResponse) WriteHeader(status int) {
	if !b.wroteHeader {
		b.status, b.wroteHeader = status, true
	}
}

// Write buffers part of the body.
func (b *bufferedResponse) Write(data []byte) (int, error) {
	b.wroteHeader = true
	return b.body.Write(data)
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip: by name at a non-zero q, or, when gzip is not
// named, by "*" at a non-zero q, as RFC 9110 reads the header.
func acceptsGzip(acceptEncoding string) bool {
	wildcard := false
	for _, entry := range strings.Split(acceptEncoding, ",") {
		name, parameters, _ := strings.Cut(strings.TrimSpace(entry), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "x-gzip" && name != "*" {
			continue
		}
		allowed := true
		if value, found := strings.CutPrefix(strings.TrimSpace(parameters), "q="); found {
			weight, err := strconv.ParseFloat(value, 64)
			allowed = err == nil && weight > 0
		}
		if name == "*" {
			wildcard = allowed
			continue
		}
		return allowed
	}
	return wildcard
}

// isCompressible reports whether a Content-Type is one Compress gzips.
func isCompressible(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && compressibleTypes[mediaType]
}

// gzipped returns body compressed with gzip.
func gzipped(body []byte) []byte {
	var compressed bytes.Buffer
	writer := gzipWriters.Get().(*gzip.Writer)
	defer func() {
		writer.Reset(io.Discard)
		gzipWriters.Put(writer)
	}()
	writer.Reset(&compressed)
	writer.Write(body)
	writer.Close()
	return compressed.Bytes()
}
