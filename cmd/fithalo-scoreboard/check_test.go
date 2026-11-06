package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// TestReplayCheckReportsTheFirstFailure proves a failed first request is reported, not the replay after it.
func TestReplayCheckReportsTheFirstFailure(t *testing.T) {
	var mutex sync.Mutex
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		authorization := request.Header.Get("Authorization")
		if seen[authorization] {
			writer.WriteHeader(http.StatusUnauthorized)
			writer.Write([]byte(`{"code":"replayed"}`))
			return
		}
		seen[authorization] = true
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	run := &checker{base: base, client: server.Client(), key: key, output: &output}

	run.checkSignatures()

	want := "FAIL  a replayed request is refused: the first request: status 503"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("got output %q, want it to contain %q", output.String(), want)
	}
}

// TestCheckReadsThePasswordFromTheEnvironment sends SCOREBOARD_CHECK_PASSWORD on join, and has no flag
// that would put the password in the process list.
func TestCheckReadsThePasswordFromTheEnvironment(t *testing.T) {
	const boardPassword = "BoardPasswordFromEnvironment"
	t.Setenv("SCOREBOARD_CHECK_PASSWORD", boardPassword)
	var mutex sync.Mutex
	sent := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == "GET" && request.URL.Path == "/api/v1/board":
			writer.Write([]byte(`{"id":"BoardBoardBoardBoard00","weekStartDay":"monday","apiVersion":"1"}`))
		case request.Method == "POST" && request.URL.Path == "/api/v1/members":
			var join struct {
				Password string `json:"password"`
			}
			body, _ := io.ReadAll(request.Body)
			json.Unmarshal(body, &join)
			mutex.Lock()
			sent = join.Password
			mutex.Unlock()
			writer.WriteHeader(http.StatusServiceUnavailable)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	var output strings.Builder
	if err := check([]string{"-url", server.URL, "-invite", "InviteCode"}, &output); err == nil {
		t.Fatalf("the check passed against a board that refuses to join:\n%s", output.String())
	}
	mutex.Lock()
	defer mutex.Unlock()
	if sent != boardPassword {
		t.Fatalf("got join password %q, want %q", sent, boardPassword)
	}
	if err := check([]string{"-url", server.URL, "-invite", "InviteCode", "-password", boardPassword}, io.Discard); err == nil {
		t.Fatal("the check accepted the board password as a flag")
	}
}
