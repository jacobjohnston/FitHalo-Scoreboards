package store

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Cosmos keeps the store in one Cosmos DB for NoSQL container through its REST API, with no SDK.
// Documents are partitioned by collection (/c), carry the data (d) and version (v), and ETags guard
// writes; each collection is also recorded in the collectionsIndex partition under collectionIndexID.
type Cosmos struct {
	// endpoint is the account's URL, without a trailing slash.
	endpoint string

	// key is the decoded account key that signs every request.
	key []byte

	// database is the database's name.
	database string

	// container is the container's name.
	container string

	// client sends the requests.
	client *http.Client

	// now returns the time each request is signed at.
	now func() time.Time
}

const (
	// cosmosAPIVersion is the REST API version every request asks for.
	cosmosAPIVersion = "2018-12-31"

	// cosmosAttempts is the most times one request is sent before giving up.
	cosmosAttempts = 6

	// cosmosBackoff is the first retry's wait when Cosmos DB names none, doubled for each retry after.
	cosmosBackoff = 100 * time.Millisecond

	// cosmosTimeout bounds each request.
	cosmosTimeout = 10 * time.Second
)

// errCosmosPrecondition is Cosmos DB's answer when a document's ETag has changed since it was read.
var errCosmosPrecondition = errors.New("store: Cosmos DB precondition failed")

// A cosmosDocument is a document, or a collection's index entry, as Cosmos DB stores it.
type cosmosDocument struct {
	// ID is the document's identifier, or collectionIndexID for an index entry.
	ID string `json:"id"`

	// Collection is the partition key: the document's collection, or collectionsIndex.
	Collection string `json:"c"`

	// Data is the document's data in standard base64.
	Data string `json:"d,omitempty"`

	// Version is the document's version.
	Version int64 `json:"v"`

	// Name is an index entry's collection.
	Name string `json:"n,omitempty"`

	// ETag is Cosmos DB's version tag, set by the server.
	ETag string `json:"_etag,omitempty"`
}

// OpenCosmos returns a store over a container, signing with the account's key.
func OpenCosmos(endpoint, key, database, container string) (*Cosmos, error) {
	if endpoint == "" || key == "" || database == "" || container == "" {
		return nil, errors.New("store: Cosmos DB needs an endpoint, a key, a database and a container")
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("store: the Cosmos DB key is not base64: %w", err)
	}
	return &Cosmos{
		endpoint: strings.TrimSuffix(endpoint, "/"), key: decoded, database: database, container: container,
		client: &http.Client{Timeout: cosmosTimeout}, now: time.Now,
	}, nil
}

// Close does nothing; the store holds no connection of its own.
func (c *Cosmos) Close() error {
	return nil
}

// collectionLink returns the container's resource link.
func (c *Cosmos) collectionLink() string {
	return "dbs/" + c.database + "/colls/" + c.container
}

// documentLink returns a document's resource link.
func (c *Cosmos) documentLink(id string) string {
	return c.collectionLink() + "/docs/" + id
}

// Get returns a document, or ErrNotFound.
func (c *Cosmos) Get(ctx context.Context, collection, id string) (Document, error) {
	if err := validateKey(collection, id); err != nil {
		return Document{}, err
	}
	stored, err := c.read(ctx, collection, id)
	if err != nil {
		return Document{}, err
	}
	return decodeCosmos(stored)
}

// Put writes a document if its version is still expectedVersion.
func (c *Cosmos) Put(ctx context.Context, collection, id string, data []byte, expectedVersion int64) (int64, error) {
	if err := validateKey(collection, id); err != nil {
		return 0, err
	}
	if err := c.recordCollection(ctx, collection); err != nil {
		return 0, err
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	for range cosmosAttempts {
		current, err := c.read(ctx, collection, id)
		switch {
		case errors.Is(err, ErrNotFound):
			if expectedVersion != 0 && expectedVersion != AnyVersion {
				return 0, ErrConflict
			}
			created := cosmosDocument{ID: id, Collection: collection, Data: encoded, Version: 1}
			err := c.send(ctx, http.MethodPost, "docs", c.collectionLink(), c.collectionLink()+"/docs", collection, created, nil, nil)
			if errors.Is(err, ErrConflict) && expectedVersion == AnyVersion {
				continue
			}
			if err != nil {
				return 0, err
			}
			return 1, nil
		case err != nil:
			return 0, err
		}
		if expectedVersion != AnyVersion && current.Version != expectedVersion {
			return 0, ErrConflict
		}
		replaced := cosmosDocument{ID: id, Collection: collection, Data: encoded, Version: current.Version + 1}
		err = c.send(ctx, http.MethodPut, "docs", c.documentLink(id), c.documentLink(id), collection, replaced,
			map[string]string{"If-Match": current.ETag}, nil)
		if errors.Is(err, errCosmosPrecondition) {
			if expectedVersion == AnyVersion {
				continue
			}
			return 0, ErrConflict
		}
		if err != nil {
			return 0, err
		}
		return replaced.Version, nil
	}
	return 0, ErrConflict
}

// Delete removes a document if its version is still expectedVersion.
func (c *Cosmos) Delete(ctx context.Context, collection, id string, expectedVersion int64) error {
	if err := validateKey(collection, id); err != nil {
		return err
	}
	headers := map[string]string{}
	if expectedVersion != AnyVersion {
		current, err := c.read(ctx, collection, id)
		if errors.Is(err, ErrNotFound) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if current.Version != expectedVersion {
			return ErrConflict
		}
		headers["If-Match"] = current.ETag
	}
	err := c.send(ctx, http.MethodDelete, "docs", c.documentLink(id), c.documentLink(id), collection, nil, headers, nil)
	switch {
	case errors.Is(err, ErrNotFound) && expectedVersion == AnyVersion:
		return nil
	case errors.Is(err, ErrNotFound), errors.Is(err, errCosmosPrecondition):
		return ErrConflict
	}
	return err
}

// List returns every document in a collection, in identifier order.
func (c *Cosmos) List(ctx context.Context, collection string) ([]Item, error) {
	if err := ValidateCollection(collection); err != nil {
		return nil, err
	}
	documents, err := c.query(ctx, collection, "SELECT * FROM c WHERE c.c = @partition", nil)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(documents))
	for _, stored := range documents {
		document, err := decodeCosmos(stored)
		if err != nil {
			return nil, err
		}
		items = append(items, Item{ID: stored.ID, Document: document})
	}
	sort.Slice(items, func(first, second int) bool { return items[first].ID < items[second].ID })
	return items, nil
}

// Collections returns the collections directly beneath prefix.
func (c *Cosmos) Collections(ctx context.Context, prefix string) ([]string, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}
	recorded, err := c.query(ctx, collectionsIndex, "SELECT * FROM c WHERE c.c = @partition AND STARTSWITH(c.n, @prefix)",
		map[string]string{"@prefix": prefix})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var collections []string
	for _, entry := range recorded {
		child, _, _ := strings.Cut(strings.TrimPrefix(entry.Name, prefix), "/")
		if child != "" && !seen[child] {
			seen[child] = true
			collections = append(collections, prefix+child)
		}
	}
	sort.Strings(collections)
	return collections, nil
}

// DeleteCollection removes a collection, every document in it, and every collection beneath it.
func (c *Cosmos) DeleteCollection(ctx context.Context, collection string) error {
	if err := ValidateCollection(collection); err != nil {
		return err
	}
	beneath, err := c.query(ctx, collectionsIndex, "SELECT * FROM c WHERE c.c = @partition AND STARTSWITH(c.n, @prefix)",
		map[string]string{"@prefix": collection + "/"})
	if err != nil {
		return err
	}
	targets := []string{collection}
	for _, entry := range beneath {
		targets = append(targets, entry.Name)
	}
	for _, target := range targets {
		documents, err := c.query(ctx, target, "SELECT * FROM c WHERE c.c = @partition", nil)
		if err != nil {
			return err
		}
		for _, stored := range documents {
			if err := c.Delete(ctx, target, stored.ID, AnyVersion); err != nil {
				return err
			}
		}
		indexID := collectionIndexID(target)
		err = c.send(ctx, http.MethodDelete, "docs", c.documentLink(indexID), c.documentLink(indexID), collectionsIndex, nil, nil, nil)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

// recordCollection notes a collection in the index, with every put, as the DynamoDB store does.
func (c *Cosmos) recordCollection(ctx context.Context, collection string) error {
	entry := cosmosDocument{ID: collectionIndexID(collection), Collection: collectionsIndex, Name: collection}
	return c.send(ctx, http.MethodPost, "docs", c.collectionLink(), c.collectionLink()+"/docs", collectionsIndex, entry,
		map[string]string{"x-ms-documentdb-is-upsert": "True"}, nil)
}

// collectionIndexID returns a collection's index entry id, base64url since an id cannot hold a slash.
func collectionIndexID(collection string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(collection))
}

// read returns a stored document with its ETag, or ErrNotFound.
func (c *Cosmos) read(ctx context.Context, collection, id string) (cosmosDocument, error) {
	var stored cosmosDocument
	err := c.send(ctx, http.MethodGet, "docs", c.documentLink(id), c.documentLink(id), collection, nil, nil, &stored)
	return stored, err
}

// query runs a query within one partition, following continuations.
func (c *Cosmos) query(ctx context.Context, partition, text string, parameters map[string]string) ([]cosmosDocument, error) {
	type parameter struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	body := map[string]any{"query": text, "parameters": []parameter{{Name: "@partition", Value: partition}}}
	for name, value := range parameters {
		body["parameters"] = append(body["parameters"].([]parameter), parameter{Name: name, Value: value})
	}
	var documents []cosmosDocument
	continuation := ""
	for {
		headers := map[string]string{"x-ms-documentdb-isquery": "True", "Content-Type": "application/query+json"}
		if continuation != "" {
			headers["x-ms-continuation"] = continuation
		}
		var output struct {
			Documents []cosmosDocument `json:"Documents"`
		}
		responseHeaders := http.Header{}
		err := c.sendWithHeaders(ctx, http.MethodPost, "docs", c.collectionLink(), c.collectionLink()+"/docs", partition, body, headers, &output, responseHeaders)
		if err != nil {
			return nil, err
		}
		documents = append(documents, output.Documents...)
		continuation = responseHeaders.Get("x-ms-continuation")
		if continuation == "" {
			return documents, nil
		}
	}
}

// send makes one REST call, ignoring the response headers.
func (c *Cosmos) send(ctx context.Context, method, resourceType, resourceLink, path, partition string, body any, headers map[string]string, output any) error {
	return c.sendWithHeaders(ctx, method, resourceType, resourceLink, path, partition, body, headers, output, nil)
}

// sendWithHeaders makes one REST call, retrying when Cosmos DB is rate limiting or briefly unavailable.
func (c *Cosmos) sendWithHeaders(ctx context.Context, method, resourceType, resourceLink, path, partition string, body any,
	headers map[string]string, output any, responseHeaders http.Header) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	partitionKey, _ := json.Marshal([]string{partition})
	for attempt := range cosmosAttempts {
		request, err := http.NewRequestWithContext(ctx, method, c.endpoint+"/"+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		date := c.now().UTC().Format(http.TimeFormat)
		request.Header.Set("x-ms-date", date)
		request.Header.Set("x-ms-version", cosmosAPIVersion)
		request.Header.Set("x-ms-documentdb-partitionkey", string(partitionKey))
		request.Header.Set("Authorization", CosmosAuthorization(c.key, method, resourceType, resourceLink, date))
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := c.client.Do(request)
		if err != nil {
			if attempt < cosmosAttempts-1 {
				continue
			}
			return err
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		switch {
		case response.StatusCode >= 200 && response.StatusCode < 300:
			if responseHeaders != nil {
				for name, values := range response.Header {
					responseHeaders[name] = values
				}
			}
			if output != nil && len(data) > 0 {
				return json.Unmarshal(data, output)
			}
			return nil
		case response.StatusCode == http.StatusNotFound:
			return ErrNotFound
		case response.StatusCode == http.StatusConflict:
			return ErrConflict
		case response.StatusCode == http.StatusPreconditionFailed:
			return errCosmosPrecondition
		case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
			wait := cosmosBackoff << attempt
			if milliseconds, err := strconv.Atoi(response.Header.Get("x-ms-retry-after-ms")); err == nil {
				wait = time.Duration(milliseconds) * time.Millisecond
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		default:
			return fmt.Errorf("store: Cosmos DB %s %s: %d %s", method, path, response.StatusCode, bytes.TrimSpace(data))
		}
	}
	return fmt.Errorf("store: Cosmos DB %s %s did not succeed after %d attempts", method, path, cosmosAttempts)
}

// CosmosAuthorization signs a request with the account key, as Cosmos DB's master-key authorization
// defines: an HMAC-SHA256 of the verb, the resource type, the resource link and the date.
func CosmosAuthorization(key []byte, method, resourceType, resourceLink, date string) string {
	text := strings.ToLower(method) + "\n" + strings.ToLower(resourceType) + "\n" + resourceLink + "\n" + strings.ToLower(date) + "\n\n"
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(text))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return url.QueryEscape("type=master&ver=1.0&sig=" + signature)
}

// decodeCosmos returns the document a stored document holds.
func decodeCosmos(stored cosmosDocument) (Document, error) {
	data, err := base64.StdEncoding.DecodeString(stored.Data)
	if err != nil {
		return Document{}, fmt.Errorf("store: a Cosmos DB document's data: %w", err)
	}
	return Document{Data: data, Version: stored.Version}, nil
}
