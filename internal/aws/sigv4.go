// Package aws signs requests to AWS with Signature Version 4, which is all the board needs to talk to
// DynamoDB without an SDK.
package aws

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// algorithm names the signing algorithm in the string to sign and the Authorization header.
	algorithm = "AWS4-HMAC-SHA256"

	// timeLayout formats the X-Amz-Date header, in UTC.
	timeLayout = "20060102T150405Z"

	// dateLayout formats the date in the credential scope, in UTC.
	dateLayout = "20060102"

	// scopeTerminator ends every credential scope and the signing key's derivation.
	scopeTerminator = "aws4_request"
)

// Credentials are an access key and its secret, with a session token when they are temporary, as a
// Lambda function's are.
type Credentials struct {
	// AccessKeyID identifies the key in the Authorization header.
	AccessKeyID string

	// SecretAccessKey derives the signing key and is never sent.
	SecretAccessKey string

	// SessionToken is the temporary credentials' token, or empty for long-term keys.
	SessionToken string
}

// CredentialsFromEnvironment reads the credentials Lambda gives a function.
func CredentialsFromEnvironment() (Credentials, error) {
	credentials := Credentials{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
	if credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" {
		return Credentials{}, errors.New("aws: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are not set")
	}
	return credentials, nil
}

// Sign adds the Authorization header and the headers it covers to request, for service in region at
// now. The body's hash is given rather than read, so the body is left for the client to send.
func Sign(request *http.Request, bodyHash string, credentials Credentials, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(timeLayout)
	request.Header.Set("X-Amz-Date", amzDate)
	if credentials.SessionToken != "" {
		request.Header.Set("X-Amz-Security-Token", credentials.SessionToken)
	}
	if request.Header.Get("Host") == "" {
		request.Header.Set("Host", request.URL.Host)
	}

	signedNames, canonicalHeaders := canonicalHeaders(request.Header)
	canonicalRequest := strings.Join([]string{
		request.Method,
		canonicalPath(request.URL),
		canonicalQuery(request.URL),
		canonicalHeaders,
		signedNames,
		bodyHash,
	}, "\n")
	scope := now.Format(dateLayout) + "/" + region + "/" + service + "/" + scopeTerminator
	stringToSign := strings.Join([]string{algorithm, amzDate, scope, hashHex([]byte(canonicalRequest))}, "\n")

	key := hmacSHA256([]byte("AWS4"+credentials.SecretAccessKey), now.Format(dateLayout))
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, scopeTerminator)
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	request.Header.Set("Authorization", algorithm+" Credential="+credentials.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedNames+", Signature="+signature)
	request.Header.Del("Host")
	request.Host = request.URL.Host
}

// HashHex is the lower-case hex SHA-256 of data, as SigV4 hashes bodies.
func HashHex(data []byte) string {
	return hashHex(data)
}

// canonicalHeaders returns the signed header names joined by semicolons, and the canonical header block.
func canonicalHeaders(header http.Header) (string, string) {
	names := make([]string, 0, len(header))
	values := map[string]string{}
	for name, headerValues := range header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "user-agent" || lower == "content-length" {
			continue
		}
		trimmed := make([]string, len(headerValues))
		for index, value := range headerValues {
			trimmed[index] = strings.Join(strings.Fields(value), " ")
		}
		names = append(names, lower)
		values[lower] = strings.Join(trimmed, ",")
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name + ":" + values[name] + "\n")
	}
	return strings.Join(names, ";"), canonical.String()
}

// canonicalPath returns the URL's escaped path, or a slash when it has none.
func canonicalPath(target *url.URL) string {
	path := target.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery returns the URL's query parameters escaped and sorted by key, then value.
func canonicalQuery(target *url.URL) string {
	query := target.Query()
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		values := query[key]
		sort.Strings(values)
		for _, value := range values {
			parts = append(parts, escape(key)+"="+escape(value))
		}
	}
	return strings.Join(parts, "&")
}

// escape percent-encodes value as SigV4 requires, with spaces as %20 rather than plus signs.
func escape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

// hashHex returns the lower-case hex SHA-256 of data.
func hashHex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// hmacSHA256 returns the HMAC-SHA256 of data under key.
func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
