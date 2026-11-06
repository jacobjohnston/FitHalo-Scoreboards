package aws

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestGetVanilla signs the get-vanilla request from AWS's Signature Version 4 test suite and checks the
// signature AWS publishes for it.
func TestGetVanilla(t *testing.T) {
	request, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	credentials := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	Sign(request, HashHex(nil), credentials, "us-east-1", "service", time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	expected := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := request.Header.Get("Authorization"); got != expected {
		t.Fatalf("authorization was\n%s", got)
	}
}

// TestSessionTokenIsSigned covers the token a Lambda function's credentials carry.
func TestSessionTokenIsSigned(t *testing.T) {
	request, _ := http.NewRequest("POST", "https://dynamodb.us-east-1.amazonaws.com/", nil)
	credentials := Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "token"}
	Sign(request, HashHex([]byte("{}")), credentials, "us-east-1", "dynamodb", time.Now())
	if !strings.Contains(request.Header.Get("Authorization"), "SignedHeaders=host;x-amz-date;x-amz-security-token") {
		t.Fatalf("the session token is not signed: %s", request.Header.Get("Authorization"))
	}
}
