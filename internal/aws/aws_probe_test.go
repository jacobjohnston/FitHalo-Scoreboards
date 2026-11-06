package aws_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/aws"
)

// suiteCredentials are the AWS SigV4 test suite's credentials.
var suiteCredentials = aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}

// suiteTime is the test suite's signing time, 20150830T123600Z.
var suiteTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

// emptyHash is the SHA-256 of nothing.
const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// authorizationPattern matches a well-formed SigV4 Authorization header.
var authorizationPattern = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/]+)/(\d{8})/([^/]+)/([^/]+)/aws4_request, SignedHeaders=([a-z0-9;-]+), Signature=([0-9a-f]{64})$`)

// hmacSHA256 is HMAC-SHA256 of data under key.
func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// sha256Hex is the lower-case hex SHA-256 of text.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// canonicalQuery sorts and encodes a query string as SigV4 does.
func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, _ := url.ParseQuery(rawQuery)
	var pairs []string
	for key, list := range values {
		for _, value := range list {
			pairs = append(pairs, sigEscape(key)+"="+sigEscape(value))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// sigEscape percent-encodes everything but the unreserved characters, as SigV4 requires.
func sigEscape(text string) string {
	var out strings.Builder
	for _, b := range []byte(text) {
		if b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_' || b == '.' || b == '~' {
			out.WriteByte(b)
			continue
		}
		out.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{b})))
	}
	return out.String()
}

// referenceSignature recomputes the SigV4 signature for request from its own headers, by the standard algorithm.
func referenceSignature(t *testing.T, request *http.Request, bodyHash string, credentials aws.Credentials, region, service string, now time.Time, signedHeaders string) string {
	t.Helper()
	var canonicalHeaders strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		value := ""
		if name == "host" {
			value = request.Host
			if value == "" {
				value = request.URL.Host
			}
		} else {
			value = strings.Join(request.Header.Values(http.CanonicalHeaderKey(name)), ",")
		}
		canonicalHeaders.WriteString(name + ":" + strings.Join(strings.Fields(value), " ") + "\n")
	}
	path := request.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonicalRequest := strings.Join([]string{request.Method, path, canonicalQuery(request.URL.RawQuery), canonicalHeaders.String(), signedHeaders, bodyHash}, "\n")
	date := now.UTC().Format("20060102")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", now.UTC().Format("20060102T150405Z"), scope, sha256Hex(canonicalRequest)}, "\n")
	key := hmacSHA256([]byte("AWS4"+credentials.SecretAccessKey), []byte(date))
	key = hmacSHA256(key, []byte(region))
	key = hmacSHA256(key, []byte(service))
	key = hmacSHA256(key, []byte("aws4_request"))
	return hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))
}

// parts splits an Authorization header into its pieces, failing the test when malformed.
func parts(t *testing.T, authorization string) (keyID, date, region, service, signedHeaders, signature string) {
	t.Helper()
	match := authorizationPattern.FindStringSubmatch(authorization)
	if match == nil {
		t.Fatalf("Authorization is malformed: %q", authorization)
	}
	return match[1], match[2], match[3], match[4], match[5], match[6]
}

// checkAgainstReference asserts that the request's signature equals the reference recomputation.
func checkAgainstReference(t *testing.T, request *http.Request, bodyHash string, credentials aws.Credentials, region, service string, now time.Time) {
	t.Helper()
	_, _, _, _, signedHeaders, signature := parts(t, request.Header.Get("Authorization"))
	if want := referenceSignature(t, request, bodyHash, credentials, region, service, now, signedHeaders); signature != want {
		t.Errorf("signature %s, reference %s (SignedHeaders %s)", signature, want, signedHeaders)
	}
}

// TestHashHex matches known SHA-256 values.
func TestHashHex(t *testing.T) {
	if got := aws.HashHex(nil); got != emptyHash {
		t.Errorf("HashHex(nil) = %s", got)
	}
	if got := aws.HashHex([]byte{}); got != emptyHash {
		t.Errorf("HashHex(empty) = %s", got)
	}
	if got := aws.HashHex([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("HashHex(abc) = %s", got)
	}
	if got := aws.HashHex([]byte("Param1=value1")); got != "9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e" {
		t.Errorf("HashHex(Param1=value1) = %s", got)
	}
	if got := aws.HashHex([]byte("ABC")); got != strings.ToLower(got) {
		t.Errorf("HashHex is not lower case: %s", got)
	}
}

// TestGetVanillaVector matches the AWS test suite's get-vanilla signature.
func TestGetVanillaVector(t *testing.T) {
	request, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	aws.Sign(request, emptyHash, suiteCredentials, "us-east-1", "service", suiteTime)
	_, date, region, service, signedHeaders, signature := parts(t, request.Header.Get("Authorization"))
	if date != "20150830" || region != "us-east-1" || service != "service" {
		t.Errorf("scope is %s/%s/%s", date, region, service)
	}
	if signedHeaders == "host;x-amz-date" {
		if signature != "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31" {
			t.Errorf("get-vanilla signature is %s", signature)
		}
	} else {
		t.Logf("SignedHeaders is %s, so the suite vector does not apply; checking against the reference instead", signedHeaders)
	}
	checkAgainstReference(t, request, emptyHash, suiteCredentials, "us-east-1", "service", suiteTime)
	if request.Header.Get("X-Amz-Date") != "20150830T123600Z" {
		t.Errorf("X-Amz-Date is %q", request.Header.Get("X-Amz-Date"))
	}
}

// TestPostVanillaVectors matches the suite's post-vanilla and post-vanilla-query signatures.
func TestPostVanillaVectors(t *testing.T) {
	cases := []struct {
		// name is the suite case.
		name string

		// url is the request URL.
		url string

		// want is the suite's signature with SignedHeaders host;x-amz-date.
		want string
	}{
		{"post-vanilla", "https://example.amazonaws.com/", "5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b"},
		{"post-vanilla-query", "https://example.amazonaws.com/?Param1=value1", "28038455d6de14eafc1f9222cf5aa6f1a96197d7deb8263271d420d138af7f11"},
	}
	for _, c := range cases {
		request, _ := http.NewRequest("POST", c.url, nil)
		aws.Sign(request, emptyHash, suiteCredentials, "us-east-1", "service", suiteTime)
		_, _, _, _, signedHeaders, signature := parts(t, request.Header.Get("Authorization"))
		if signedHeaders == "host;x-amz-date" && signature != c.want {
			t.Errorf("%s: signature %s, want %s", c.name, signature, c.want)
		}
		checkAgainstReference(t, request, emptyHash, suiteCredentials, "us-east-1", "service", suiteTime)
	}
}

// TestSignStructure checks the Authorization header's shape and the headers it adds.
func TestSignStructure(t *testing.T) {
	request, _ := http.NewRequest("POST", "https://dynamodb.us-west-2.amazonaws.com/", strings.NewReader(`{"TableName":"board"}`))
	request.Header.Set("Content-Type", "application/x-amz-json-1.0")
	request.Header.Set("X-Amz-Target", "DynamoDB_20120810.GetItem")
	now := time.Date(2026, 2, 28, 23, 59, 59, 999_999_999, time.FixedZone("CET", 3600))
	bodyHash := aws.HashHex([]byte(`{"TableName":"board"}`))
	credentials := aws.Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "secret", SessionToken: "FwoGZXIvYXdzEBYaDHtoken"}
	aws.Sign(request, bodyHash, credentials, "us-west-2", "dynamodb", now)
	keyID, date, region, service, signedHeaders, _ := parts(t, request.Header.Get("Authorization"))
	if keyID != "AKIAIOSFODNN7EXAMPLE" || date != "20260228" || region != "us-west-2" || service != "dynamodb" {
		t.Errorf("credential scope is %s/%s/%s/%s", keyID, date, region, service)
	}
	if request.Header.Get("X-Amz-Date") != "20260228T225959Z" {
		t.Errorf("X-Amz-Date is %q, want UTC basic format", request.Header.Get("X-Amz-Date"))
	}
	if request.Header.Get("X-Amz-Security-Token") != credentials.SessionToken {
		t.Errorf("X-Amz-Security-Token is %q", request.Header.Get("X-Amz-Security-Token"))
	}
	names := strings.Split(signedHeaders, ";")
	if !sort.StringsAreSorted(names) {
		t.Errorf("SignedHeaders %s is not sorted", signedHeaders)
	}
	if signedHeaders != strings.ToLower(signedHeaders) {
		t.Errorf("SignedHeaders %s is not lower case", signedHeaders)
	}
	for _, required := range []string{"host", "x-amz-date", "x-amz-security-token"} {
		if !strings.Contains(";"+signedHeaders+";", ";"+required+";") {
			t.Errorf("SignedHeaders %s lacks %s", signedHeaders, required)
		}
	}
	// PROBE: DynamoDB requires content-type and x-amz-target in the signature only when sent; they are sent here.
	t.Logf("SignedHeaders: %s", signedHeaders)
	checkAgainstReference(t, request, bodyHash, credentials, "us-west-2", "dynamodb", now)
}

// TestSignWithoutSessionToken sends no security token for long-term keys.
func TestSignWithoutSessionToken(t *testing.T) {
	request, _ := http.NewRequest("POST", "https://dynamodb.us-east-1.amazonaws.com/", nil)
	aws.Sign(request, emptyHash, aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}, "us-east-1", "dynamodb", suiteTime)
	if request.Header.Get("X-Amz-Security-Token") != "" {
		t.Errorf("X-Amz-Security-Token %q was sent without a session token", request.Header.Get("X-Amz-Security-Token"))
	}
	_, _, _, _, signedHeaders, _ := parts(t, request.Header.Get("Authorization"))
	if strings.Contains(signedHeaders, "security-token") {
		t.Errorf("SignedHeaders %s names an absent token", signedHeaders)
	}
}

// TestSignIsDeterministic produces the same header for the same inputs.
func TestSignIsDeterministic(t *testing.T) {
	sign := func() string {
		request, _ := http.NewRequest("POST", "https://dynamodb.us-east-1.amazonaws.com/", nil)
		request.Header.Set("Content-Type", "application/x-amz-json-1.0")
		aws.Sign(request, emptyHash, suiteCredentials, "us-east-1", "dynamodb", suiteTime)
		return request.Header.Get("Authorization")
	}
	if sign() != sign() {
		t.Error("two signatures of the same request differ")
	}
}

// TestSignChangesWithInputs changes the signature when any signed input changes.
func TestSignChangesWithInputs(t *testing.T) {
	base := func(mutate func(*http.Request) (string, aws.Credentials, string, string, time.Time)) string {
		request, _ := http.NewRequest("POST", "https://dynamodb.us-east-1.amazonaws.com/", nil)
		request.Header.Set("Content-Type", "application/x-amz-json-1.0")
		bodyHash, credentials, region, service, now := mutate(request)
		aws.Sign(request, bodyHash, credentials, region, service, now)
		_, _, _, _, _, signature := parts(t, request.Header.Get("Authorization"))
		return signature
	}
	same := func(*http.Request) (string, aws.Credentials, string, string, time.Time) {
		return emptyHash, suiteCredentials, "us-east-1", "dynamodb", suiteTime
	}
	reference := base(same)
	mutations := map[string]func(*http.Request) (string, aws.Credentials, string, string, time.Time){
		"body hash": func(*http.Request) (string, aws.Credentials, string, string, time.Time) {
			return aws.HashHex([]byte("x")), suiteCredentials, "us-east-1", "dynamodb", suiteTime
		},
		"signed header value": func(request *http.Request) (string, aws.Credentials, string, string, time.Time) {
			request.Header.Set("Content-Type", "application/json")
			return same(request)
		},
		"region": func(*http.Request) (string, aws.Credentials, string, string, time.Time) {
			return emptyHash, suiteCredentials, "eu-west-1", "dynamodb", suiteTime
		},
		"service": func(*http.Request) (string, aws.Credentials, string, string, time.Time) {
			return emptyHash, suiteCredentials, "us-east-1", "s3", suiteTime
		},
		"time by a second": func(*http.Request) (string, aws.Credentials, string, string, time.Time) {
			return emptyHash, suiteCredentials, "us-east-1", "dynamodb", suiteTime.Add(time.Second)
		},
		"secret": func(*http.Request) (string, aws.Credentials, string, string, time.Time) {
			return emptyHash, aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "other"}, "us-east-1", "dynamodb", suiteTime
		},
		"host": func(request *http.Request) (string, aws.Credentials, string, string, time.Time) {
			request.URL.Host = "dynamodb.us-east-2.amazonaws.com"
			return same(request)
		},
		"path": func(request *http.Request) (string, aws.Credentials, string, string, time.Time) {
			request.URL.Path = "/other"
			return same(request)
		},
		"query": func(request *http.Request) (string, aws.Credentials, string, string, time.Time) {
			request.URL.RawQuery = "a=1"
			return same(request)
		},
		"method": func(request *http.Request) (string, aws.Credentials, string, string, time.Time) {
			request.Method = "PUT"
			return same(request)
		},
	}
	for name, mutate := range mutations {
		if base(mutate) == reference {
			t.Errorf("changing the %s left the signature unchanged", name)
		}
	}
}

// TestSignCanonicalisesQueryAndHeaders agrees with the reference over awkward queries and header spacing.
func TestSignCanonicalisesQueryAndHeaders(t *testing.T) {
	cases := []struct {
		// name names the case.
		name string

		// url is the request URL.
		url string

		// headers are extra headers to set before signing.
		headers map[string]string
	}{
		{"unsorted query", "https://example.amazonaws.com/?b=2&a=1&a=0", nil},
		{"query needing encoding", "https://example.amazonaws.com/?key=a%20b&sym=%2A%7E", nil},
		{"empty value", "https://example.amazonaws.com/?empty=&x=1", nil},
		{"header with inner spaces", "https://example.amazonaws.com/", map[string]string{"X-Amz-Target": "a   b  c"}},
		{"header with outer spaces", "https://example.amazonaws.com/", map[string]string{"X-Amz-Target": "  trimmed  "}},
		{"mixed-case header name", "https://example.amazonaws.com/", map[string]string{"x-AMZ-target": "value"}},
		{"path with segments", "https://example.amazonaws.com/a/b/c", nil},
	}
	for _, c := range cases {
		request, _ := http.NewRequest("POST", c.url, nil)
		for name, value := range c.headers {
			request.Header.Set(name, value)
		}
		aws.Sign(request, emptyHash, suiteCredentials, "us-east-1", "service", suiteTime)
		_, _, _, _, signedHeaders, signature := parts(t, request.Header.Get("Authorization"))
		if want := referenceSignature(t, request, emptyHash, suiteCredentials, "us-east-1", "service", suiteTime, signedHeaders); signature != want {
			t.Errorf("%s: signature %s, reference %s (SignedHeaders %s)", c.name, signature, want, signedHeaders)
		}
	}
}

// TestSignTwiceReplacesHeaders signs a request a second time without doubling headers.
func TestSignTwiceReplacesHeaders(t *testing.T) {
	request, _ := http.NewRequest("POST", "https://dynamodb.us-east-1.amazonaws.com/", nil)
	aws.Sign(request, emptyHash, suiteCredentials, "us-east-1", "dynamodb", suiteTime)
	aws.Sign(request, emptyHash, suiteCredentials, "us-east-1", "dynamodb", suiteTime.Add(time.Hour))
	if values := request.Header.Values("Authorization"); len(values) != 1 {
		t.Errorf("Authorization has %d values after signing twice", len(values))
	}
	if values := request.Header.Values("X-Amz-Date"); len(values) != 1 || values[0] != "20150830T133600Z" {
		t.Errorf("X-Amz-Date is %v after signing twice", values)
	}
	checkAgainstReference(t, request, emptyHash, suiteCredentials, "us-east-1", "dynamodb", suiteTime.Add(time.Hour))
}

// TestSignedHostIsTheHostSent signs the same host the client will put on the wire.
func TestSignedHostIsTheHostSent(t *testing.T) {
	// PROBE: Sign replaces a caller's request.Host with URL.Host, so the two agree; a caller wanting a
	// different Host header than the URL's cannot have it. The board never does, so this only records it.
	request, _ := http.NewRequest("POST", "https://10.0.0.1/", nil)
	request.Host = "dynamodb.us-east-1.amazonaws.com"
	aws.Sign(request, emptyHash, suiteCredentials, "us-east-1", "dynamodb", suiteTime)
	t.Logf("after Sign request.Host is %q (URL.Host %q)", request.Host, request.URL.Host)
	checkAgainstReference(t, request, emptyHash, suiteCredentials, "us-east-1", "dynamodb", suiteTime)
}

// TestCredentialsFromEnvironment reads the three variables and requires the first two.
func TestCredentialsFromEnvironment(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "token")
	credentials, err := aws.CredentialsFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if credentials != (aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "token"}) {
		t.Errorf("credentials are %+v", credentials)
	}
	t.Setenv("AWS_SESSION_TOKEN", "")
	credentials, err = aws.CredentialsFromEnvironment()
	if err != nil || credentials.SessionToken != "" {
		t.Errorf("without a token: %+v, %v", credentials, err)
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if _, err := aws.CredentialsFromEnvironment(); err == nil {
		t.Error("a missing secret was accepted")
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	if _, err := aws.CredentialsFromEnvironment(); err == nil {
		t.Error("a missing key id was accepted")
	}
}
