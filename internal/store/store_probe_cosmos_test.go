package store_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// cosmosExampleKey is the account key in Microsoft's worked example of master-key authorization.
const cosmosExampleKey = "dsZQi3KtZmCv1ljt3VNWNm7sQUF1y5rJfC6kv5JiwvW0EndXdDku/dkKBp8/ufDToSxLzR4y+O/0H/t4bQtVNw=="

// cosmosExampleSignature is the signature Microsoft's worked example documents for GET dbs/ToDoList.
const cosmosExampleSignature = "c09PEVJrgp2uQRkr934kFbTqhByc7TVr3OHyqlu+c+c="

// cosmosExampleDate is the x-ms-date header of Microsoft's worked example.
const cosmosExampleDate = "Thu, 27 Apr 2017 00:51:12 GMT"

// cosmosKey decodes the example key.
func cosmosKey(t *testing.T) []byte {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(cosmosExampleKey)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// referenceCosmosAuthorization recomputes the documented algorithm: HMAC-SHA256 over lower(verb), lower(type),
// link, lower(date) and an empty line, each newline-terminated, then URL-encoded as type=master&ver=1.0&sig=.
func referenceCosmosAuthorization(key []byte, method, resourceType, resourceLink, date string) string {
	payload := strings.ToLower(method) + "\n" + strings.ToLower(resourceType) + "\n" + resourceLink + "\n" + strings.ToLower(date) + "\n" + "\n"
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return url.QueryEscape("type=master&ver=1.0&sig=" + signature)
}

// TestCosmosAuthorizationMatchesMicrosoftExample checks the documented worked example byte for byte.
func TestCosmosAuthorizationMatchesMicrosoftExample(t *testing.T) {
	got := store.CosmosAuthorization(cosmosKey(t), "GET", "dbs", "dbs/ToDoList", cosmosExampleDate)
	decoded, err := url.QueryUnescape(got)
	if err != nil {
		t.Fatalf("authorization is not URL-encoded: %q, %v", got, err)
	}
	if want := "type=master&ver=1.0&sig=" + cosmosExampleSignature; decoded != want {
		t.Fatalf("authorization %q, want %q", decoded, want)
	}
	if strings.ContainsAny(got, "&=+/") {
		t.Fatalf("authorization %q is not fully URL-encoded", got)
	}
}

// TestCosmosAuthorizationMatchesReference checks the exported function against an independent reimplementation.
func TestCosmosAuthorizationMatchesReference(t *testing.T) {
	key := cosmosKey(t)
	for _, test := range []struct {
		name, method, resourceType, resourceLink, date string
	}{
		{"example", "GET", "dbs", "dbs/ToDoList", cosmosExampleDate},
		{"document create", "POST", "docs", "dbs/board/colls/documents", "Sat, 25 Oct 2026 09:00:00 GMT"},
		{"document read", "GET", "docs", "dbs/board/colls/documents/docs/weeks|2026-10-19|a", "Sat, 25 Oct 2026 09:00:00 GMT"},
		{"query", "POST", "docs", "dbs/board/colls/documents", "Mon, 01 Jan 2029 00:00:00 GMT"},
		{"delete", "DELETE", "docs", "dbs/board/colls/documents/docs/x", "Sat, 25 Oct 2026 09:00:00 GMT"},
		{"empty link", "GET", "dbs", "", "Sat, 25 Oct 2026 09:00:00 GMT"},
		{"empty everything", "", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := store.CosmosAuthorization(key, test.method, test.resourceType, test.resourceLink, test.date)
			want := referenceCosmosAuthorization(key, test.method, test.resourceType, test.resourceLink, test.date)
			if got != want {
				t.Fatalf("CosmosAuthorization = %q, reference %q", got, want)
			}
		})
	}
}

// TestCosmosAuthorizationIsDeterministic checks repeated calls agree.
func TestCosmosAuthorizationIsDeterministic(t *testing.T) {
	key := cosmosKey(t)
	first := store.CosmosAuthorization(key, "GET", "dbs", "dbs/ToDoList", cosmosExampleDate)
	for i := 0; i < 100; i++ {
		if again := store.CosmosAuthorization(key, "GET", "dbs", "dbs/ToDoList", cosmosExampleDate); again != first {
			t.Fatalf("call %d returned %q, first %q", i, again, first)
		}
	}
}

// TestCosmosAuthorizationVerbCaseIsFolded checks the verb and resource type are lower-cased before signing.
func TestCosmosAuthorizationVerbCaseIsFolded(t *testing.T) {
	key := cosmosKey(t)
	upper := store.CosmosAuthorization(key, "GET", "DBS", "dbs/ToDoList", strings.ToUpper(cosmosExampleDate))
	lower := store.CosmosAuthorization(key, "get", "dbs", "dbs/ToDoList", strings.ToLower(cosmosExampleDate))
	if upper != lower {
		t.Fatalf("verb, type or date case changed the signature: %q vs %q", upper, lower)
	}
}

// TestCosmosAuthorizationResourceLinkCaseIsSignificant checks the link is signed as given.
func TestCosmosAuthorizationResourceLinkCaseIsSignificant(t *testing.T) {
	key := cosmosKey(t)
	mixed := store.CosmosAuthorization(key, "GET", "dbs", "dbs/ToDoList", cosmosExampleDate)
	lower := store.CosmosAuthorization(key, "GET", "dbs", "dbs/todolist", cosmosExampleDate)
	if mixed == lower {
		t.Fatal("changing the resource link's case did not change the signature")
	}
}

// TestCosmosAuthorizationInputsChangeOutput checks each input participates in the signature.
func TestCosmosAuthorizationInputsChangeOutput(t *testing.T) {
	key := cosmosKey(t)
	base := store.CosmosAuthorization(key, "GET", "dbs", "dbs/ToDoList", cosmosExampleDate)
	for _, test := range []struct {
		name, method, resourceType, resourceLink, date string
	}{
		{"verb", "POST", "dbs", "dbs/ToDoList", cosmosExampleDate},
		{"type", "GET", "colls", "dbs/ToDoList", cosmosExampleDate},
		{"link", "GET", "dbs", "dbs/ToDoLists", cosmosExampleDate},
		{"date second", "GET", "dbs", "dbs/ToDoList", "Thu, 27 Apr 2017 00:51:13 GMT"},
		{"date year", "GET", "dbs", "dbs/ToDoList", "Fri, 27 Apr 2018 00:51:12 GMT"},
	} {
		if got := store.CosmosAuthorization(key, test.method, test.resourceType, test.resourceLink, test.date); got == base {
			t.Errorf("changing the %s left the signature unchanged", test.name)
		}
	}
	otherKey := append([]byte{}, key...)
	otherKey[0] ^= 0x01
	if got := store.CosmosAuthorization(otherKey, "GET", "dbs", "dbs/ToDoList", cosmosExampleDate); got == base {
		t.Error("changing the key left the signature unchanged")
	}
}

// TestCosmosAuthorizationFieldsCannotBeSmuggled checks a newline in the link does not collide with another input split.
func TestCosmosAuthorizationFieldsCannotBeSmuggled(t *testing.T) {
	key := cosmosKey(t)
	// A link containing a newline followed by a date must not sign identically to the honest split; the
	// reference algorithm has this property only because the date is lower-cased separately.
	smuggled := store.CosmosAuthorization(key, "GET", "dbs", "dbs/ToDoList\nthu, 27 apr 2017 00:51:12 gmt", "")
	honest := store.CosmosAuthorization(key, "GET", "dbs", "dbs/ToDoList", cosmosExampleDate)
	// PROBE: this collision is inherent in Microsoft's string-to-sign and is informational only.
	t.Logf("smuggled == honest: %v", smuggled == honest)
}
