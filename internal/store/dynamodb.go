package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/aws"
)

// DynamoDB keeps the store in one table through DynamoDB's JSON API, signed with SigV4 and no SDK.
// Items hold the collection (c, partition key), identifier (i, sort key), data (d) and version (v); each
// collection is also recorded in the collectionsIndex partition, so Collections needs no scan.
type DynamoDB struct {
	// table is the table's name.
	table string

	// region is the AWS region the table is in.
	region string

	// endpoint is the URL every operation is posted to.
	endpoint string

	// credentials returns the credentials to sign each request with.
	credentials func() (aws.Credentials, error)

	// client sends the requests.
	client *http.Client

	// now returns the time each request is signed at.
	now func() time.Time
}

const (
	// collectionsIndex is the partition that records every collection.
	collectionsIndex = "_collections"

	// dynamoAttempts is the most times one operation is sent before giving up.
	dynamoAttempts = 6

	// dynamoBackoff is the wait before the first retry, doubled for each one after.
	dynamoBackoff = 50 * time.Millisecond

	// dynamoTimeout bounds each request.
	dynamoTimeout = 10 * time.Second
)

// OpenDynamoDB returns a store over table in region, signing with the credentials Lambda provides.
func OpenDynamoDB(table, region string) (*DynamoDB, error) {
	if table == "" || region == "" {
		return nil, errors.New("store: DynamoDB needs a table and a region")
	}
	return &DynamoDB{
		table:       table,
		region:      region,
		endpoint:    "https://dynamodb." + region + ".amazonaws.com/",
		credentials: aws.CredentialsFromEnvironment,
		client:      &http.Client{Timeout: dynamoTimeout},
		now:         time.Now,
	}, nil
}

// Close does nothing; the store holds no connection of its own.
func (db *DynamoDB) Close() error {
	return nil
}

// An attribute is one DynamoDB attribute value in the JSON API's typed form.
type attribute struct {
	// S is a string value.
	S string `json:"S,omitempty"`

	// N is a number value, in decimal.
	N string `json:"N,omitempty"`

	// B is a binary value, in standard base64.
	B string `json:"B,omitempty"`
}

// An item is a DynamoDB item, keyed by attribute name.
type item map[string]attribute

// key returns the primary key of a document.
func (db *DynamoDB) key(collection, id string) item {
	return item{"c": {S: collection}, "i": {S: id}}
}

// Get returns a document, or ErrNotFound.
func (db *DynamoDB) Get(ctx context.Context, collection, id string) (Document, error) {
	if err := validateKey(collection, id); err != nil {
		return Document{}, err
	}
	var output struct {
		Item item `json:"Item"`
	}
	err := db.call(ctx, "GetItem", map[string]any{"TableName": db.table, "Key": db.key(collection, id), "ConsistentRead": true}, &output)
	if err != nil {
		return Document{}, err
	}
	if output.Item == nil {
		return Document{}, ErrNotFound
	}
	return decodeItem(output.Item)
}

// Put writes a document if its version is still expectedVersion.
func (db *DynamoDB) Put(ctx context.Context, collection, id string, data []byte, expectedVersion int64) (int64, error) {
	if err := validateKey(collection, id); err != nil {
		return 0, err
	}
	if err := db.recordCollection(ctx, collection); err != nil {
		return 0, err
	}
	values := map[string]attribute{":d": {B: base64.StdEncoding.EncodeToString(data)}, ":one": {N: "1"}}
	input := map[string]any{
		"TableName":                 db.table,
		"Key":                       db.key(collection, id),
		"UpdateExpression":          "SET #d = :d ADD #v :one",
		"ExpressionAttributeNames":  map[string]string{"#d": "d", "#v": "v"},
		"ExpressionAttributeValues": values,
		"ReturnValues":              "UPDATED_NEW",
	}
	switch {
	case expectedVersion == 0:
		input["ConditionExpression"] = "attribute_not_exists(#c)"
		input["ExpressionAttributeNames"] = map[string]string{"#c": "c", "#d": "d", "#v": "v"}
	case expectedVersion != AnyVersion:
		input["ConditionExpression"] = "#v = :expected"
		values[":expected"] = attribute{N: strconv.FormatInt(expectedVersion, 10)}
	}
	var output struct {
		Attributes item `json:"Attributes"`
	}
	if err := db.call(ctx, "UpdateItem", input, &output); err != nil {
		return 0, err
	}
	return strconv.ParseInt(output.Attributes["v"].N, 10, 64)
}

// Delete removes a document if its version is still expectedVersion.
func (db *DynamoDB) Delete(ctx context.Context, collection, id string, expectedVersion int64) error {
	if err := validateKey(collection, id); err != nil {
		return err
	}
	input := map[string]any{"TableName": db.table, "Key": db.key(collection, id)}
	if expectedVersion != AnyVersion {
		input["ConditionExpression"] = "#v = :expected"
		input["ExpressionAttributeNames"] = map[string]string{"#v": "v"}
		input["ExpressionAttributeValues"] = map[string]attribute{":expected": {N: strconv.FormatInt(expectedVersion, 10)}}
	}
	return db.call(ctx, "DeleteItem", input, nil)
}

// List returns every document in a collection, in identifier order.
func (db *DynamoDB) List(ctx context.Context, collection string) ([]Item, error) {
	if err := ValidateCollection(collection); err != nil {
		return nil, err
	}
	items, err := db.query(ctx, collection, "")
	if err != nil {
		return nil, err
	}
	result := make([]Item, 0, len(items))
	for _, found := range items {
		document, err := decodeItem(found)
		if err != nil {
			return nil, err
		}
		result = append(result, Item{ID: found["i"].S, Document: document})
	}
	return result, nil
}

// Collections returns the collections directly beneath prefix.
func (db *DynamoDB) Collections(ctx context.Context, prefix string) ([]string, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}
	recorded, err := db.query(ctx, collectionsIndex, prefix)
	if err != nil {
		return nil, err
	}
	var collections []string
	seen := map[string]bool{}
	for _, entry := range recorded {
		remainder := strings.TrimPrefix(entry["i"].S, prefix)
		child, _, _ := strings.Cut(remainder, "/")
		if child != "" && !seen[child] {
			seen[child] = true
			collections = append(collections, prefix+child)
		}
	}
	return collections, nil
}

// DeleteCollection removes a collection, every document in it, and every collection beneath it.
func (db *DynamoDB) DeleteCollection(ctx context.Context, collection string) error {
	if err := ValidateCollection(collection); err != nil {
		return err
	}
	beneath, err := db.query(ctx, collectionsIndex, collection+"/")
	if err != nil {
		return err
	}
	targets := []string{collection}
	for _, entry := range beneath {
		targets = append(targets, entry["i"].S)
	}
	for _, target := range targets {
		items, err := db.query(ctx, target, "")
		if err != nil {
			return err
		}
		for _, found := range items {
			if err := db.call(ctx, "DeleteItem", map[string]any{"TableName": db.table, "Key": db.key(target, found["i"].S)}, nil); err != nil {
				return err
			}
		}
		if err := db.call(ctx, "DeleteItem", map[string]any{"TableName": db.table, "Key": db.key(collectionsIndex, target)}, nil); err != nil {
			return err
		}
	}
	return nil
}

// recordCollection notes a collection in the index on every put, not cached, so a collection another
// instance deleted is recorded again.
func (db *DynamoDB) recordCollection(ctx context.Context, collection string) error {
	return db.call(ctx, "PutItem", map[string]any{"TableName": db.table, "Item": db.key(collectionsIndex, collection)}, nil)
}

// query returns every item in a partition, or only those whose identifier begins with prefix.
func (db *DynamoDB) query(ctx context.Context, partition, prefix string) ([]item, error) {
	var items []item
	var startKey item
	for {
		input := map[string]any{
			"TableName":                 db.table,
			"KeyConditionExpression":    "#c = :c",
			"ExpressionAttributeNames":  map[string]string{"#c": "c"},
			"ExpressionAttributeValues": map[string]attribute{":c": {S: partition}},
			"ConsistentRead":            true,
		}
		if prefix != "" {
			input["KeyConditionExpression"] = "#c = :c AND begins_with(#i, :prefix)"
			input["ExpressionAttributeNames"] = map[string]string{"#c": "c", "#i": "i"}
			input["ExpressionAttributeValues"] = map[string]attribute{":c": {S: partition}, ":prefix": {S: prefix}}
		}
		if startKey != nil {
			input["ExclusiveStartKey"] = startKey
		}
		var output struct {
			Items            []item `json:"Items"`
			LastEvaluatedKey item   `json:"LastEvaluatedKey"`
		}
		if err := db.call(ctx, "Query", input, &output); err != nil {
			return nil, err
		}
		items = append(items, output.Items...)
		if output.LastEvaluatedKey == nil {
			return items, nil
		}
		startKey = output.LastEvaluatedKey
	}
}

// call sends one DynamoDB operation, retrying when DynamoDB is throttling or briefly unavailable.
func (db *DynamoDB) call(ctx context.Context, operation string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	var lastFailure error
	for attempt := range dynamoAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(dynamoBackoff << attempt):
			}
		}
		credentials, err := db.credentials()
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, db.endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/x-amz-json-1.0")
		request.Header.Set("X-Amz-Target", "DynamoDB_20120810."+operation)
		aws.Sign(request, aws.HashHex(body), credentials, db.region, "dynamodb", db.now())
		response, err := db.client.Do(request)
		if err != nil {
			lastFailure = err
			continue
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			if output == nil {
				return nil
			}
			return json.Unmarshal(data, output)
		}
		var failure struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		json.Unmarshal(data, &failure)
		kind := failure.Type[strings.LastIndex(failure.Type, "#")+1:]
		switch {
		case kind == "ConditionalCheckFailedException":
			return ErrConflict
		case kind == "ProvisionedThroughputExceededException" || kind == "ThrottlingException" ||
			kind == "RequestLimitExceeded" || response.StatusCode >= 500:
			lastFailure = fmt.Errorf("%d %s %s", response.StatusCode, kind, failure.Message)
			continue
		default:
			return fmt.Errorf("store: DynamoDB %s: %s %s", operation, kind, failure.Message)
		}
	}
	return fmt.Errorf("store: DynamoDB %s did not succeed after %d attempts: %w", operation, dynamoAttempts,
		lastFailure)
}

// decodeItem returns the document an item holds.
func decodeItem(found item) (Document, error) {
	data, err := base64.StdEncoding.DecodeString(found["d"].B)
	if err != nil {
		return Document{}, fmt.Errorf("store: a DynamoDB item's data: %w", err)
	}
	version, err := strconv.ParseInt(found["v"].N, 10, 64)
	if err != nil {
		return Document{}, fmt.Errorf("store: a DynamoDB item's version: %w", err)
	}
	return Document{Data: data, Version: version}, nil
}
