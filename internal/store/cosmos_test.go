package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCosmosSignature checks the signature against the worked example in Microsoft's documentation of
// Cosmos DB's master-key authorization.
func TestCosmosSignature(t *testing.T) {
	key, _ := base64.StdEncoding.DecodeString("dsZQi3KtZmCv1ljt3VNWNm7sQUF1y5rJfC6kv5JiwvW0EndXdDku/dkKBp8/ufDToSxLzR4y+O/0H/t4bQtVNw==")
	got, _ := url.QueryUnescape(CosmosAuthorization(key, "GET", "dbs", "dbs/ToDoList", "Thu, 27 Apr 2017 00:51:12 GMT"))
	if expected := "type=master&ver=1.0&sig=c09PEVJrgp2uQRkr934kFbTqhByc7TVr3OHyqlu+c+c="; got != expected {
		t.Fatalf("authorization %s", got)
	}
}

// fakeCosmos answers the subset of Cosmos DB's REST API the store uses, with ETags, upserts and query
// pages of two, and throttles the first request with a 429.
type fakeCosmos struct {
	// t reports requests the fake does not expect.
	t *testing.T

	// mutex guards every field below it.
	mutex sync.Mutex

	// documents holds each partition's documents by id.
	documents map[string]map[string]map[string]any

	// etag is the last ETag issued.
	etag int

	// throttled is whether the first request has been throttled yet.
	throttled bool
}

// ServeHTTP answers one REST call.
func (f *fakeCosmos) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if !strings.HasPrefix(request.Header.Get("Authorization"), "type%3Dmaster%26ver%3D1.0%26sig%3D") || request.Header.Get("x-ms-date") == "" {
		f.t.Errorf("an unsigned request: %v", request.Header)
	}
	if !f.throttled {
		f.throttled = true
		writer.Header().Set("x-ms-retry-after-ms", "1")
		writer.WriteHeader(http.StatusTooManyRequests)
		return
	}
	var partition []string
	json.Unmarshal([]byte(request.Header.Get("x-ms-documentdb-partitionkey")), &partition)
	documents := f.documents[partition[0]]
	if documents == nil {
		documents = map[string]map[string]any{}
		f.documents[partition[0]] = documents
	}
	body, _ := io.ReadAll(request.Body)
	id := ""
	if index := strings.Index(request.URL.Path, "/docs/"); index >= 0 {
		id = request.URL.Path[index+len("/docs/"):]
	}

	switch {
	case request.Method == http.MethodGet:
		if document, found := documents[id]; found {
			json.NewEncoder(writer).Encode(document)
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	case request.Method == http.MethodPost && request.Header.Get("x-ms-documentdb-isquery") == "True":
		var query struct {
			Query      string `json:"query"`
			Parameters []struct{ Name, Value string }
		}
		json.Unmarshal(body, &query)
		prefix := ""
		for _, parameter := range query.Parameters {
			if parameter.Name == "@prefix" {
				prefix = parameter.Value
			}
		}
		var ids []string
		for documentID, document := range documents {
			if name, _ := document["n"].(string); strings.Contains(query.Query, "STARTSWITH") && !strings.HasPrefix(name, prefix) {
				continue
			}
			ids = append(ids, documentID)
		}
		sort.Strings(ids)
		start := 0
		fmt.Sscan(request.Header.Get("x-ms-continuation"), &start)
		page := []map[string]any{}
		for index := start; index < len(ids) && index < start+2; index++ {
			page = append(page, documents[ids[index]])
		}
		if start+2 < len(ids) {
			writer.Header().Set("x-ms-continuation", fmt.Sprint(start+2))
		}
		json.NewEncoder(writer).Encode(map[string]any{"Documents": page})
	case request.Method == http.MethodPost:
		var document map[string]any
		json.Unmarshal(body, &document)
		documentID := document["id"].(string)
		if _, exists := documents[documentID]; exists && request.Header.Get("x-ms-documentdb-is-upsert") != "True" {
			writer.WriteHeader(http.StatusConflict)
			return
		}
		f.save(documents, documentID, document)
		writer.WriteHeader(http.StatusCreated)
	case request.Method == http.MethodPut:
		existing, found := documents[id]
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if match := request.Header.Get("If-Match"); match != "" && match != existing["_etag"] {
			writer.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		var document map[string]any
		json.Unmarshal(body, &document)
		f.save(documents, id, document)
	case request.Method == http.MethodDelete:
		existing, found := documents[id]
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if match := request.Header.Get("If-Match"); match != "" && match != existing["_etag"] {
			writer.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		delete(documents, id)
		writer.WriteHeader(http.StatusNoContent)
	}
}

// save stores document under id with a fresh ETag.
func (f *fakeCosmos) save(documents map[string]map[string]any, id string, document map[string]any) {
	f.etag++
	document["_etag"] = fmt.Sprintf(`"%d"`, f.etag)
	documents[id] = document
}

// openFakeCosmos returns a Cosmos store wired to a fresh fake, with a fixed clock.
func openFakeCosmos(t *testing.T) Store {
	t.Helper()
	fake := &fakeCosmos{t: t, documents: map[string]map[string]map[string]any{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cosmos, err := OpenCosmos(server.URL, base64.StdEncoding.EncodeToString([]byte("account key")), "scoreboard", "board")
	if err != nil {
		t.Fatal(err)
	}
	cosmos.now = func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }
	return cosmos
}

// TestCosmos holds the Cosmos DB backend to the store's contract, against the fake.
func TestCosmos(t *testing.T) {
	runContract(t, openFakeCosmos)
}
