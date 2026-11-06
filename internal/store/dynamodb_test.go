package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/aws"
)

// fakeDynamoDB answers the part of DynamoDB's JSON API the store uses, paging Query results two at a
// time and throttling the first request so retries are exercised too.
type fakeDynamoDB struct {
	// t reports requests the fake does not expect.
	t *testing.T

	// mutex guards every field below it.
	mutex sync.Mutex

	// items holds each partition's items by identifier.
	items map[string]map[string]item

	// requests counts the requests received.
	requests int

	// throttled is whether the first request has been throttled yet.
	throttled bool
}

// ServeHTTP answers one DynamoDB operation.
func (f *fakeDynamoDB) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.requests++
	if !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKID/") ||
		request.Header.Get("Content-Type") != "application/x-amz-json-1.0" {
		f.t.Errorf("an unsigned or mistyped request: %v", request.Header)
	}
	if !f.throttled {
		f.throttled = true
		f.fail(writer, "ProvisionedThroughputExceededException")
		return
	}
	var input struct {
		Key                       item                 `json:"Key"`
		Item                      item                 `json:"Item"`
		ConditionExpression       string               `json:"ConditionExpression"`
		ExpressionAttributeValues map[string]attribute `json:"ExpressionAttributeValues"`
		KeyConditionExpression    string               `json:"KeyConditionExpression"`
		ExclusiveStartKey         item                 `json:"ExclusiveStartKey"`
	}
	json.NewDecoder(request.Body).Decode(&input)
	operation := strings.TrimPrefix(request.Header.Get("X-Amz-Target"), "DynamoDB_20120810.")
	switch operation {
	case "GetItem":
		existing, found := f.lookup(input.Key)
		if found {
			f.reply(writer, map[string]any{"Item": existing})
		} else {
			f.reply(writer, map[string]any{})
		}
	case "PutItem":
		f.store(input.Item)
		f.reply(writer, map[string]any{})
	case "UpdateItem":
		existing, found := f.lookup(input.Key)
		if !f.conditionHolds(input.ConditionExpression, input.ExpressionAttributeValues, existing, found) {
			f.fail(writer, "ConditionalCheckFailedException")
			return
		}
		version := int64(0)
		if found {
			version, _ = strconv.ParseInt(existing["v"].N, 10, 64)
		}
		updated := item{"c": input.Key["c"], "i": input.Key["i"], "d": input.ExpressionAttributeValues[":d"], "v": {N: strconv.FormatInt(version+1, 10)}}
		f.store(updated)
		f.reply(writer, map[string]any{"Attributes": item{"d": updated["d"], "v": updated["v"]}})
	case "DeleteItem":
		existing, found := f.lookup(input.Key)
		if !f.conditionHolds(input.ConditionExpression, input.ExpressionAttributeValues, existing, found) {
			f.fail(writer, "ConditionalCheckFailedException")
			return
		}
		delete(f.items[input.Key["c"].S], input.Key["i"].S)
		f.reply(writer, map[string]any{})
	case "Query":
		partition := input.ExpressionAttributeValues[":c"].S
		prefix := input.ExpressionAttributeValues[":prefix"].S
		var ids []string
		for id := range f.items[partition] {
			if strings.HasPrefix(id, prefix) && (input.ExclusiveStartKey == nil || id > input.ExclusiveStartKey["i"].S) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		output := map[string]any{}
		page := []item{}
		for index, id := range ids {
			if index == 2 {
				output["LastEvaluatedKey"] = item{"c": {S: partition}, "i": {S: ids[1]}}
				break
			}
			page = append(page, f.items[partition][id])
		}
		output["Items"] = page
		f.reply(writer, output)
	default:
		f.t.Errorf("unexpected operation %s", operation)
		f.fail(writer, "UnknownOperationException")
	}
}

// conditionHolds evaluates one of the condition expressions the store sends.
func (f *fakeDynamoDB) conditionHolds(condition string, values map[string]attribute, existing item, found bool) bool {
	switch condition {
	case "":
		return true
	case "attribute_not_exists(#c)":
		return !found
	case "#v = :expected":
		return found && existing["v"].N == values[":expected"].N
	}
	f.t.Errorf("unexpected condition %q", condition)
	return false
}

// lookup returns the item with key, and whether it exists.
func (f *fakeDynamoDB) lookup(key item) (item, bool) {
	found, exists := f.items[key["c"].S][key["i"].S]
	return found, exists
}

// store saves value under its own key.
func (f *fakeDynamoDB) store(value item) {
	if f.items[value["c"].S] == nil {
		f.items[value["c"].S] = map[string]item{}
	}
	f.items[value["c"].S][value["i"].S] = value
}

// reply writes value as a successful response.
func (f *fakeDynamoDB) reply(writer http.ResponseWriter, value any) {
	json.NewEncoder(writer).Encode(value)
}

// fail writes a DynamoDB error of kind.
func (f *fakeDynamoDB) fail(writer http.ResponseWriter, kind string) {
	writer.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(writer).Encode(map[string]string{"__type": "com.amazonaws.dynamodb.v20120810#" + kind, "message": kind})
}

// openFakeDynamoDB returns a DynamoDB store wired to a fresh fake, with fixed credentials and clock.
func openFakeDynamoDB(t *testing.T) Store {
	t.Helper()
	fake := &fakeDynamoDB{t: t, items: map[string]map[string]item{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	database, err := OpenDynamoDB("scoreboard", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	database.endpoint = server.URL + "/"
	database.credentials = func() (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "token"}, nil
	}
	database.now = func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }
	return database
}

// TestDynamoDB holds the DynamoDB backend to the store's contract, against the fake.
func TestDynamoDB(t *testing.T) {
	runContract(t, openFakeDynamoDB)
}

// TestDynamoDBKeepsTheLastFailure proves giving up reports why the last attempt failed.
func TestDynamoDBKeepsTheLastFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	database, err := OpenDynamoDB("scoreboard", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	database.endpoint = server.URL + "/"
	database.credentials = func() (aws.Credentials, error) { return aws.Credentials{AccessKeyID: "AKID"}, nil }

	_, err = database.Get(context.Background(), "board", "board")
	var transportFailure *url.Error
	if !errors.As(err, &transportFailure) {
		t.Fatalf("Get() = %v, want the transport error wrapped", err)
	}
}
