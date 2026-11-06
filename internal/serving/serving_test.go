package serving

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCertificate writes a self-signed localhost certificate and key with serial, both dated modified.
func writeCertificate(t *testing.T, directory string, serial int64, modified time.Time) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	certificatePath, keyPath := filepath.Join(directory, "certificate.pem"), filepath.Join(directory, "key.pem")
	os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	os.Chtimes(certificatePath, modified, modified)
	os.Chtimes(keyPath, modified, modified)
	return certificatePath, keyPath
}

// TestRenewalIsPickedUp serves a renewed certificate after the next check, without a restart.
func TestRenewalIsPickedUp(t *testing.T) {
	directory := t.TempDir()
	started := time.Now().Add(-time.Hour)
	certificatePath, keyPath := writeCertificate(t, directory, 1, started)
	reloader, err := NewCertificateReloader(certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	moment := time.Now()
	reloader.now = func() time.Time { return moment }
	serial := func() int64 {
		certificate, _ := reloader.GetCertificate(nil)
		parsed, _ := x509.ParseCertificate(certificate.Certificate[0])
		return parsed.SerialNumber.Int64()
	}
	if serial() != 1 {
		t.Fatal("the first certificate was not served")
	}
	writeCertificate(t, directory, 2, started.Add(time.Minute))
	moment = moment.Add(reloadCheckInterval)
	if serial() != 2 {
		t.Fatal("the renewed certificate was not picked up")
	}
}

// TestOldTLSIsRefused refuses TLS 1.0 and 1.1 handshakes and accepts 1.2 and 1.3.
func TestOldTLSIsRefused(t *testing.T) {
	certificatePath, keyPath := writeCertificate(t, t.TempDir(), 1, time.Now())
	reloader, err := NewCertificateReloader(certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Write([]byte("ok"))
	}))
	server.TLS = TLSConfig(reloader)
	server.StartTLS()
	defer server.Close()

	address := server.Listener.Addr().String()
	for version, shouldConnect := range map[uint16]bool{tls.VersionTLS10: false, tls.VersionTLS11: false, tls.VersionTLS12: true, tls.VersionTLS13: true} {
		connection, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true, MinVersion: version, MaxVersion: version})
		if connection != nil {
			connection.Close()
		}
		if (err == nil) != shouldConnect {
			t.Errorf("TLS version %x: connected %v, want %v (%v)", version, err == nil, shouldConnect, err)
		}
	}
}

// TestProxyMustSayHTTPS refuses a request the proxy did not receive over HTTPS, and reads the client's
// address from the proxy's own X-Forwarded-For entry.
func TestProxyMustSayHTTPS(t *testing.T) {
	handler := RequireProxyHTTPS(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {}))
	for proto, expected := range map[string]int{"": http.StatusMisdirectedRequest, "http": http.StatusMisdirectedRequest, "https": http.StatusOK, "HTTPS": http.StatusOK} {
		request := httptest.NewRequest("GET", "/", nil)
		if proto != "" {
			request.Header.Set("X-Forwarded-Proto", proto)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != expected {
			t.Errorf("X-Forwarded-Proto %q: %d, want %d", proto, recorder.Code, expected)
		}
	}
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.4")
	if address := ProxyClientAddress(request); address != "198.51.100.4" {
		t.Errorf("the client address behind a proxy was %s", address)
	}
}

// TestListenAddresses keeps plain HTTP off public addresses.
func TestListenAddresses(t *testing.T) {
	cases := []struct {
		mode, address string
		allowed       bool
	}{
		{ModeDevelopment, "127.0.0.1:8080", true},
		{ModeDevelopment, "0.0.0.0:8080", false},
		{ModeProxy, "127.0.0.1:8080", true},
		{ModeProxy, "0.0.0.0:8080", true},
		{ModeProxy, ":8080", true},
		{ModeProxy, "192.168.1.20:8080", true},
		{ModeProxy, "203.0.113.9:8080", false},
		{ModeFiles, "0.0.0.0:443", true},
		{"plaintext", "127.0.0.1:80", false},
	}
	for _, testCase := range cases {
		if err := CheckListenAddress(testCase.mode, testCase.address); (err == nil) != testCase.allowed {
			t.Errorf("%s on %s: %v", testCase.mode, testCase.address, err)
		}
	}
}

// TestProxyClientAddress takes the proxy's own X-Forwarded-For entry, and the peer when there is none.
func TestProxyClientAddress(t *testing.T) {
	cases := []struct {
		forwarded, remote, want string
	}{
		{"", "192.0.2.1:5000", "192.0.2.1"},
		{"203.0.113.9", "192.0.2.1:5000", "203.0.113.9"},
		{"203.0.113.9, 198.51.100.4", "192.0.2.1:5000", "198.51.100.4"},
		{"203.0.113.9, ", "192.0.2.1:5000", "192.0.2.1"},
		{"2001:db8::7", "[::1]:5000", "2001:db8::7"},
		{"", "unix", "unix"},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest("GET", "/", nil)
		request.RemoteAddr = testCase.remote
		if testCase.forwarded != "" {
			request.Header.Set("X-Forwarded-For", testCase.forwarded)
		}
		if got := ProxyClientAddress(request); got != testCase.want {
			t.Errorf("ProxyClientAddress(%q from %s) = %s, want %s", testCase.forwarded, testCase.remote, got, testCase.want)
		}
	}
}

// TestParseTrustedProxies reads CIDRs and single addresses, and refuses anything else.
func TestParseTrustedProxies(t *testing.T) {
	trusted, err := ParseTrustedProxies("10.0.0.0/8, 192.0.2.7 ::1,2001:db8::/32")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "192.0.2.7/32", "::1/128", "2001:db8::/32"}
	if len(trusted) != len(want) {
		t.Fatalf("got %v, want %v", trusted, want)
	}
	for index, prefix := range trusted {
		if prefix.String() != want[index] {
			t.Errorf("entry %d = %s, want %s", index, prefix, want[index])
		}
	}
	if empty, err := ParseTrustedProxies(" "); err != nil || len(empty) != 0 {
		t.Errorf("ParseTrustedProxies(blank) = %v, %v, want none", empty, err)
	}
	for _, bad := range []string{"10.0.0.0/33", "proxy.example.com", "10.0.0"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Errorf("ParseTrustedProxies(%q) was accepted", bad)
		}
	}
}

// TestUntrustedPeerIsRefused answers 421 to a peer outside the trusted proxies, and passes every peer when
// none are set.
func TestUntrustedPeerIsRefused(t *testing.T) {
	trusted, _ := ParseTrustedProxies("172.16.0.0/12,::1")
	cases := []struct {
		trusted TrustedProxies
		remote  string
		want    int
	}{
		{trusted, "172.17.0.1:40000", http.StatusOK},
		{trusted, "[::1]:40000", http.StatusOK},
		{trusted, "[::ffff:172.17.0.1]:40000", http.StatusOK},
		{trusted, "203.0.113.9:40000", http.StatusMisdirectedRequest},
		{trusted, "[2001:db8::1]:40000", http.StatusMisdirectedRequest},
		{nil, "203.0.113.9:40000", http.StatusOK},
	}
	for _, testCase := range cases {
		handler := testCase.trusted.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		request := httptest.NewRequest("GET", "/", nil)
		request.RemoteAddr = testCase.remote
		request.Header.Set("X-Forwarded-Proto", "https")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != testCase.want {
			t.Errorf("peer %s with trusted %v: %d, want %d", testCase.remote, testCase.trusted, recorder.Code, testCase.want)
		}
	}
}

// TestTrustedClientAddress believes X-Forwarded-For only from a trusted peer, skipping trusted hops.
func TestTrustedClientAddress(t *testing.T) {
	trusted, _ := ParseTrustedProxies("10.0.0.0/8")
	cases := []struct {
		trusted                 TrustedProxies
		forwarded, remote, want string
	}{
		{trusted, "198.51.100.4", "10.0.0.2:5000", "198.51.100.4"},
		{trusted, "203.0.113.9, 198.51.100.4, 10.0.0.3", "10.0.0.2:5000", "198.51.100.4"},
		{trusted, "10.0.0.4, 10.0.0.3", "10.0.0.2:5000", "10.0.0.4"},
		{trusted, "", "10.0.0.2:5000", "10.0.0.2"},
		{trusted, "198.51.100.4", "203.0.113.9:5000", "203.0.113.9"},
		{nil, "198.51.100.4", "203.0.113.9:5000", "198.51.100.4"},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest("GET", "/", nil)
		request.RemoteAddr = testCase.remote
		if testCase.forwarded != "" {
			request.Header.Set("X-Forwarded-For", testCase.forwarded)
		}
		if got := testCase.trusted.ClientAddress(request); got != testCase.want {
			t.Errorf("ClientAddress(%q from %s) = %s, want %s", testCase.forwarded, testCase.remote, got, testCase.want)
		}
	}
}
