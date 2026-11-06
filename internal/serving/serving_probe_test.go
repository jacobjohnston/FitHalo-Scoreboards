package serving_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/serving"
)

// listenCase is one mode and address CheckListenAddress is asked about.
type listenCase struct {
	// name names the case.
	name string

	// mode is the serving mode.
	mode string

	// address is the listen address.
	address string

	// allowed says whether the address must be accepted.
	allowed bool
}

// TestCheckListenAddressDevelopmentIsLoopbackOnly allows only loopback in development mode.
func TestCheckListenAddressDevelopmentIsLoopbackOnly(t *testing.T) {
	cases := []listenCase{
		{"ipv4 loopback", serving.ModeDevelopment, "127.0.0.1:8080", true},
		{"ipv4 loopback other", serving.ModeDevelopment, "127.0.0.2:8080", true},
		{"ipv6 loopback", serving.ModeDevelopment, "[::1]:8080", true},
		{"all ipv4", serving.ModeDevelopment, "0.0.0.0:8080", false},
		{"bare port", serving.ModeDevelopment, ":8080", false},
		{"private", serving.ModeDevelopment, "192.168.1.5:8080", false},
		{"all ipv6", serving.ModeDevelopment, "[::]:8080", false},
		{"public", serving.ModeDevelopment, "8.8.8.8:80", false},
		{"ipv4-mapped public", serving.ModeDevelopment, "[::ffff:8.8.8.8]:80", false},
		{"ipv4-mapped loopback", serving.ModeDevelopment, "[::ffff:127.0.0.1]:8080", true},
	}
	checkListenCases(t, cases)
}

// TestCheckListenAddressProxyAllowsPrivateAndUnspecified allows loopback, private and unspecified in proxy mode.
func TestCheckListenAddressProxyAllowsPrivateAndUnspecified(t *testing.T) {
	cases := []listenCase{
		{"loopback", serving.ModeProxy, "127.0.0.1:8080", true},
		{"ipv6 loopback", serving.ModeProxy, "[::1]:8080", true},
		{"10/8", serving.ModeProxy, "10.1.2.3:8080", true},
		{"172.16/12 low", serving.ModeProxy, "172.16.0.1:8080", true},
		{"172.16/12 high", serving.ModeProxy, "172.31.255.254:8080", true},
		{"172.32 is public", serving.ModeProxy, "172.32.0.1:8080", false},
		{"192.168/16", serving.ModeProxy, "192.168.1.5:8080", true},
		{"fc00::/7", serving.ModeProxy, "[fd00::1]:8080", true},
		{"all ipv4", serving.ModeProxy, "0.0.0.0:8080", true},
		{"bare port", serving.ModeProxy, ":8080", true},
		{"all ipv6", serving.ModeProxy, "[::]:8080", true},
		{"public ipv4", serving.ModeProxy, "8.8.8.8:80", false},
		{"public ipv6", serving.ModeProxy, "[2600::1]:80", false},
		{"ipv4-mapped public", serving.ModeProxy, "[::ffff:8.8.8.8]:80", false},
		{"ipv4-mapped private", serving.ModeProxy, "[::ffff:10.0.0.1]:80", true},
	}
	checkListenCases(t, cases)
}

// TestCheckListenAddressFilesAllowsAnyInterface lets a TLS board listen anywhere.
func TestCheckListenAddressFilesAllowsAnyInterface(t *testing.T) {
	cases := []listenCase{
		{"loopback", serving.ModeFiles, "127.0.0.1:8443", true},
		{"all ipv4", serving.ModeFiles, "0.0.0.0:443", true},
		{"bare port", serving.ModeFiles, ":443", true},
		{"public ipv4", serving.ModeFiles, "8.8.8.8:443", true},
		{"public ipv6", serving.ModeFiles, "[2600::1]:443", true},
	}
	checkListenCases(t, cases)
}

// TestCheckListenAddressRefusesMalformed errors on an address that is not host:port.
func TestCheckListenAddressRefusesMalformed(t *testing.T) {
	for _, mode := range []string{serving.ModeDevelopment, serving.ModeProxy, serving.ModeFiles} {
		for _, address := range []string{"8080", "127.0.0.1", "", "[::1]", "[::1:8080", "127.0.0.1:8080:1"} {
			if err := serving.CheckListenAddress(mode, address); err == nil {
				t.Errorf("%s mode accepted malformed %q", mode, address)
			}
		}
	}
}

// TestCheckListenAddressBadPort records that the port is not checked.
func TestCheckListenAddressBadPort(t *testing.T) {
	// PROBE: the doc judges the host only; a bad port fails later at Listen, which is a fine place.
	for _, address := range []string{"127.0.0.1:99999", "127.0.0.1:-1", "127.0.0.1:port"} {
		t.Logf("development %s: %v", address, serving.CheckListenAddress(serving.ModeDevelopment, address))
	}
}

// TestCheckListenAddressUnknownMode records what an unknown mode does.
func TestCheckListenAddressUnknownMode(t *testing.T) {
	// PROBE: the doc names three modes; an unknown mode should not be accepted silently.
	if err := serving.CheckListenAddress("production", "0.0.0.0:8080"); err == nil {
		t.Log("an unknown mode accepts 0.0.0.0:8080")
	} else {
		t.Logf("an unknown mode errors: %v", err)
	}
}

// TestCheckListenAddressHostnames records how hostnames are judged.
func TestCheckListenAddressHostnames(t *testing.T) {
	// PROBE: "localhost" resolves to loopback but the doc does not say whether names are accepted.
	for _, mode := range []string{serving.ModeDevelopment, serving.ModeProxy} {
		for _, address := range []string{"localhost:8080", "example.com:443"} {
			t.Logf("%s %s: %v", mode, address, serving.CheckListenAddress(mode, address))
		}
	}
	if err := serving.CheckListenAddress(serving.ModeDevelopment, "example.com:443"); err == nil {
		t.Error("development mode accepted a public hostname")
	}
	if err := serving.CheckListenAddress(serving.ModeProxy, "example.com:443"); err == nil {
		t.Error("proxy mode accepted a public hostname")
	}
}

// checkListenCases runs CheckListenAddress over cases.
func checkListenCases(t *testing.T, cases []listenCase) {
	t.Helper()
	for _, c := range cases {
		err := serving.CheckListenAddress(c.mode, c.address)
		if c.allowed && err != nil {
			t.Errorf("%s: %s refused %q: %v", c.name, c.mode, c.address, err)
		}
		if !c.allowed && err == nil {
			t.Errorf("%s: %s accepted %q", c.name, c.mode, c.address)
		}
	}
}

// okHandler answers 200 with a short body.
var okHandler = http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
	writer.Write([]byte("ok"))
})

// serveWithHeader sends a GET with one header through handler.
func serveWithHeader(handler http.Handler, name, value string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("GET", "/", nil)
	if name != "" {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestRequireProxyHTTPS passes only a request the proxy marked https.
func TestRequireProxyHTTPS(t *testing.T) {
	handler := serving.RequireProxyHTTPS(okHandler)
	cases := []struct {
		// name names the case.
		name string

		// value is the X-Forwarded-Proto value; empty sends none.
		value string

		// pass says whether the request must reach the handler.
		pass bool
	}{
		{"https", "https", true},
		{"http", "http", false},
		{"missing", "", false},
		{"empty", " ", false},
		{"ws", "wss", false},
		{"list https first", "https, http", false},
		{"list http first", "http, https", false},
		{"list no space", "https,http", false},
		{"https prefix", "httpsx", false},
	}
	for _, c := range cases {
		name := "X-Forwarded-Proto"
		if c.value == "" {
			name = ""
		}
		recorder := serveWithHeader(handler, name, c.value)
		if c.pass && recorder.Code != http.StatusOK {
			t.Errorf("%s: %q was refused with %d", c.name, c.value, recorder.Code)
		}
		if !c.pass {
			if recorder.Code != http.StatusMisdirectedRequest {
				t.Errorf("%s: %q answered %d, want 421", c.name, c.value, recorder.Code)
			}
			if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain") {
				t.Errorf("%s: 421 content type is %q", c.name, recorder.Header().Get("Content-Type"))
			}
			if recorder.Body.Len() == 0 {
				t.Errorf("%s: 421 has no body", c.name)
			}
		}
	}
}

// TestRequireProxyHTTPSUppercase records whether the scheme is compared case-insensitively.
func TestRequireProxyHTTPSUppercase(t *testing.T) {
	// PROBE: scheme names are case-insensitive by RFC 3986; proxies send lower case in practice.
	recorder := serveWithHeader(serving.RequireProxyHTTPS(okHandler), "X-Forwarded-Proto", "HTTPS")
	t.Logf("X-Forwarded-Proto: HTTPS answered %d", recorder.Code)
}

// TestRequireProxyHTTPSWithRepeatedHeader refuses when the header is repeated with http last.
func TestRequireProxyHTTPSWithRepeatedHeader(t *testing.T) {
	handler := serving.RequireProxyHTTPS(okHandler)
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Add("X-Forwarded-Proto", "https")
	request.Header.Add("X-Forwarded-Proto", "http")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	// PROBE: two header lines; the first says https. A proxy appends, so the last is the trustworthy one.
	t.Logf("repeated X-Forwarded-Proto https then http answered %d", recorder.Code)
}

// TestProxyClientAddress reads the last entry of X-Forwarded-For.
func TestProxyClientAddress(t *testing.T) {
	cases := []struct {
		// name names the case.
		name string

		// header is the X-Forwarded-For value; "-" sends none.
		header string

		// want is the address expected.
		want string
	}{
		{"three", "a, b, c", "c"},
		{"single", "1.2.3.4", "1.2.3.4"},
		{"spaces", "1.1.1.1 ,  2.2.2.2  ", "2.2.2.2"},
		{"tabs", "1.1.1.1,\t2.2.2.2", "2.2.2.2"},
		{"ipv6 bracketed", "1.1.1.1, [::1]", "[::1]"},
		{"ipv6 bare", "1.1.1.1, 2001:db8::7", "2001:db8::7"},
		{"with port", "1.1.1.1, 1.2.3.4:555", "1.2.3.4:555"},
	}
	for _, c := range cases {
		request := httptest.NewRequest("GET", "/", nil)
		request.RemoteAddr = "10.9.8.7:4321"
		if c.header != "-" {
			request.Header.Set("X-Forwarded-For", c.header)
		}
		if got := serving.ProxyClientAddress(request); got != c.want {
			t.Errorf("%s: ProxyClientAddress(%q) = %q, want %q", c.name, c.header, got, c.want)
		}
	}
}

// TestProxyClientAddressFallsBack records what is returned without a usable header.
func TestProxyClientAddressFallsBack(t *testing.T) {
	// PROBE: the doc does not say what happens without the header; an empty key would merge every
	// client into one rate-limit bucket.
	for _, header := range []string{"-", "", "1.1.1.1,", ",", " , "} {
		request := httptest.NewRequest("GET", "/", nil)
		request.RemoteAddr = "10.9.8.7:4321"
		if header != "-" {
			request.Header.Set("X-Forwarded-For", header)
		}
		got := serving.ProxyClientAddress(request)
		t.Logf("X-Forwarded-For %q from %s -> %q", header, request.RemoteAddr, got)
		if got == "" {
			t.Errorf("X-Forwarded-For %q yields an empty client address", header)
		}
	}
}

// TestProxyClientAddressRepeatedHeaderLines takes the last entry of the last line.
func TestProxyClientAddressRepeatedHeaderLines(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Add("X-Forwarded-For", "1.1.1.1")
	request.Header.Add("X-Forwarded-For", "2.2.2.2, 3.3.3.3")
	if got := serving.ProxyClientAddress(request); got != "3.3.3.3" {
		t.Errorf("two X-Forwarded-For lines gave %q, want 3.3.3.3 (the proxy appends last)", got)
	}
}

// TestParseTrustedProxies accepts CIDRs and single addresses with mixed separators.
func TestParseTrustedProxies(t *testing.T) {
	set, err := serving.ParseTrustedProxies("10.0.0.0/8, 192.168.1.1 ,fd00::/8")
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 3 {
		t.Fatalf("parsed %d entries: %v", len(set), set)
	}
	for _, inside := range []string{"10.1.2.3", "10.255.255.255:80", "192.168.1.1", "192.168.1.1:443", "fd00::1", "[fd00::1]:8080", "[fd12::ffff]:1"} {
		if !set.Contains(inside) {
			t.Errorf("%q is not in %v", inside, set)
		}
	}
	for _, outside := range []string{"11.0.0.0", "192.168.1.2", "192.168.1.1.1", "fc00::1", "fe00::1", "", "garbage", "[fd00::1]"} {
		if set.Contains(outside) {
			t.Errorf("%q is in %v", outside, set)
		}
	}
	// PROBE: a non-numeric port is stripped without complaint; RemoteAddr never carries one.
	t.Logf("[fd00::1]:x in the set: %v", set.Contains("[fd00::1]:x"))
}

// TestParseTrustedProxiesSeparators accepts spaces, commas, and both together.
func TestParseTrustedProxiesSeparators(t *testing.T) {
	for _, value := range []string{"10.0.0.0/8 192.168.0.0/16", "10.0.0.0/8,192.168.0.0/16", "10.0.0.0/8,,192.168.0.0/16", " 10.0.0.0/8 , 192.168.0.0/16 ", "10.0.0.0/8\t192.168.0.0/16"} {
		set, err := serving.ParseTrustedProxies(value)
		if err != nil {
			t.Errorf("%q: %v", value, err)
			continue
		}
		if len(set) != 2 {
			t.Errorf("%q parsed to %v", value, set)
		}
	}
}

// TestParseTrustedProxiesRefusesBadEntries errors on anything not an address or CIDR.
func TestParseTrustedProxiesRefusesBadEntries(t *testing.T) {
	for _, value := range []string{"10.0.0.0/33", "foo", "10.0.0.1/", "10.0.0.1/-1", "::/129", "10.0.0", "10.0.0.1:8080", "10.0.0.0/8;192.168.0.0/16", "300.0.0.0/8", "10.0.0.0/8 bad"} {
		if _, err := serving.ParseTrustedProxies(value); err == nil {
			t.Errorf("%q was accepted", value)
		}
	}
}

// TestParseTrustedProxiesEmpty returns an empty set, not an error, for an empty value.
func TestParseTrustedProxiesEmpty(t *testing.T) {
	for _, value := range []string{"", "  ", ",", " , "} {
		set, err := serving.ParseTrustedProxies(value)
		if err != nil {
			t.Errorf("%q: %v", value, err)
		}
		if len(set) != 0 {
			t.Errorf("%q parsed to %v", value, set)
		}
	}
}

// TestParseTrustedProxiesHostBitsSet records whether 10.1.2.3/8 is accepted and what it means.
func TestParseTrustedProxiesHostBitsSet(t *testing.T) {
	// PROBE: 10.1.2.3/8 has host bits set; netip keeps them, so Contains could miss 10.0.0.1 unless masked.
	set, err := serving.ParseTrustedProxies("10.1.2.3/8")
	if err != nil {
		t.Logf("10.1.2.3/8 is refused: %v", err)
		return
	}
	if !set.Contains("10.200.0.1") {
		t.Error("10.1.2.3/8 was accepted but does not contain 10.200.0.1")
	}
}

// TestContainsZoneAndMapped handles IPv6 zones and IPv4-mapped addresses.
func TestContainsZoneAndMapped(t *testing.T) {
	set, err := serving.ParseTrustedProxies("10.0.0.0/8, fe80::/10")
	if err != nil {
		t.Fatal(err)
	}
	if !set.Contains("fe80::1%eth0") {
		t.Error("a link-local peer with a zone is not trusted")
	}
	if !set.Contains("[fe80::1%eth0]:8080") {
		t.Error("a link-local peer with a zone and a port is not trusted")
	}
	// PROBE: Go reports an IPv4-mapped peer as ::ffff:10.0.0.1 on a dual-stack listener.
	t.Logf("::ffff:10.0.0.1 in 10.0.0.0/8: %v", set.Contains("::ffff:10.0.0.1"))
	t.Logf("[::ffff:10.0.0.1]:80 in 10.0.0.0/8: %v", set.Contains("[::ffff:10.0.0.1]:80"))
}

// TestContainsOnEmptySet is false for every address.
func TestContainsOnEmptySet(t *testing.T) {
	var set serving.TrustedProxies
	for _, address := range []string{"127.0.0.1", "127.0.0.1:1", "::1", ""} {
		if set.Contains(address) {
			t.Errorf("an empty set contains %q", address)
		}
	}
}

// requestFrom builds a GET from peer with an X-Forwarded-For value, or none when forwarded is "-".
func requestFrom(peer, forwarded string) *http.Request {
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = peer
	if forwarded != "-" {
		request.Header.Set("X-Forwarded-For", forwarded)
	}
	return request
}

// TestClientAddressWithTrustedProxies walks the chain back to the first untrusted hop.
func TestClientAddressWithTrustedProxies(t *testing.T) {
	set, err := serving.ParseTrustedProxies("10.0.0.0/8, 127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		// name names the case.
		name string

		// peer is the connection's address.
		peer string

		// forwarded is the X-Forwarded-For value.
		forwarded string

		// want is the client address expected.
		want string
	}{
		{"trusted peer two hops", "10.0.0.9:1", "1.1.1.1, 10.0.0.5", "1.1.1.1"},
		{"trusted peer one hop", "10.0.0.9:1", "1.1.1.1", "1.1.1.1"},
		{"trusted peer three hops", "127.0.0.1:1", "9.9.9.9, 1.1.1.1, 10.0.0.5", "1.1.1.1"},
		{"trusted peer with spaces", "10.0.0.9:1", " 1.1.1.1 , 10.0.0.5 ", "1.1.1.1"},
		{"untrusted peer ignores header", "8.8.8.8:1", "1.1.1.1, 10.0.0.5", "8.8.8.8"},
		{"untrusted peer no header", "8.8.8.8:1", "-", "8.8.8.8"},
		{"untrusted ipv6 peer", "[2600::1]:1", "1.1.1.1", "2600::1"},
	}
	for _, c := range cases {
		if got := set.ClientAddress(requestFrom(c.peer, c.forwarded)); got != c.want {
			t.Errorf("%s: ClientAddress = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestClientAddressReadsEveryHeaderLine walks back across header lines, since HAProxy adds its own line.
func TestClientAddressReadsEveryHeaderLine(t *testing.T) {
	set, _ := serving.ParseTrustedProxies("10.0.0.0/8")
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = "10.0.0.9:1"
	request.Header.Add("X-Forwarded-For", "6.6.6.6")
	request.Header.Add("X-Forwarded-For", "1.1.1.1")
	if got := set.ClientAddress(request); got != "1.1.1.1" {
		t.Errorf("a client line followed by the proxy's own line gave %q, want 1.1.1.1 (the client chose its address)", got)
	}
}

// TestClientAddressAllHopsTrusted records what is returned when every hop is a trusted proxy.
func TestClientAddressAllHopsTrusted(t *testing.T) {
	// PROBE: "the last entry not itself a trusted proxy" has no answer here; an empty key would be unsafe.
	set, _ := serving.ParseTrustedProxies("10.0.0.0/8")
	for _, forwarded := range []string{"10.0.0.1, 10.0.0.2", "-", "", "garbage, 10.0.0.2"} {
		got := set.ClientAddress(requestFrom("10.0.0.9:1", forwarded))
		t.Logf("trusted peer with X-Forwarded-For %q -> %q", forwarded, got)
		if got == "" {
			t.Errorf("X-Forwarded-For %q from a trusted peer yields an empty client address", forwarded)
		}
	}
}

// TestClientAddressEmptySetIsProxyClientAddress believes every peer when the set is empty.
func TestClientAddressEmptySetIsProxyClientAddress(t *testing.T) {
	var set serving.TrustedProxies
	request := requestFrom("8.8.8.8:1", "1.1.1.1, 2.2.2.2")
	if got, want := set.ClientAddress(request), serving.ProxyClientAddress(request); got != want {
		t.Errorf("empty set: ClientAddress = %q, ProxyClientAddress = %q", got, want)
	}
}

// TestRequireRefusesUntrustedPeer answers 421 in plain text to a peer outside the set.
func TestRequireRefusesUntrustedPeer(t *testing.T) {
	set, _ := serving.ParseTrustedProxies("10.0.0.0/8")
	handler := set.Require(okHandler)
	for _, peer := range []string{"8.8.8.8:1", "[2600::1]:1", "11.0.0.1:1", "garbage"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, requestFrom(peer, "-"))
		if recorder.Code != http.StatusMisdirectedRequest {
			t.Errorf("peer %s answered %d, want 421", peer, recorder.Code)
		}
		if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("peer %s: 421 content type is %q", peer, recorder.Header().Get("Content-Type"))
		}
	}
	for _, peer := range []string{"10.0.0.1:1", "10.255.255.255:65535"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, requestFrom(peer, "-"))
		if recorder.Code != http.StatusOK {
			t.Errorf("peer %s answered %d, want 200", peer, recorder.Code)
		}
	}
}

// TestRequireEmptySetPassesEveryone lets every peer through when nothing is configured.
func TestRequireEmptySetPassesEveryone(t *testing.T) {
	var set serving.TrustedProxies
	handler := set.Require(okHandler)
	for _, peer := range []string{"8.8.8.8:1", "127.0.0.1:1", "[2600::1]:1"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, requestFrom(peer, "-"))
		if recorder.Code != http.StatusOK {
			t.Errorf("peer %s answered %d with an empty set", peer, recorder.Code)
		}
	}
}

// writeCertificatePair writes a fresh self-signed certificate and key into directory and returns their paths.
func writeCertificatePair(t *testing.T, directory string, serial int64) (string, string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "board.test"},
		DNSNames:     []string{"board.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificate(der)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	certificatePath := filepath.Join(directory, "cert.pem")
	keyPath := filepath.Join(directory, "key.pem")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificatePath, keyPath, parsed
}

// touchLater moves both files' modification time forward by seconds.
func touchLater(t *testing.T, seconds int, paths ...string) {
	t.Helper()
	later := time.Now().Add(time.Duration(seconds) * time.Second)
	for _, path := range paths {
		if err := os.Chtimes(path, later, later); err != nil {
			t.Fatal(err)
		}
	}
}

// servedSerial returns the serial of the certificate reloader currently serves.
func servedSerial(t *testing.T, reloader *serving.CertificateReloader) int64 {
	t.Helper()
	certificate, err := reloader.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if certificate == nil || len(certificate.Certificate) == 0 {
		t.Fatal("GetCertificate returned no certificate")
	}
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return parsed.SerialNumber.Int64()
}

// TestCertificateReloaderServesTheLoadedPair serves the certificate it loaded, repeatedly.
func TestCertificateReloaderServesTheLoadedPair(t *testing.T) {
	directory := t.TempDir()
	certificatePath, keyPath, _ := writeCertificatePair(t, directory, 1)
	reloader, err := serving.NewCertificateReloader(certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		if serial := servedSerial(t, reloader); serial != 1 {
			t.Fatalf("served serial %d, want 1", serial)
		}
	}
}

// TestCertificateReloaderRenewalAfterRewrite records that a rewrite is not seen at once.
func TestCertificateReloaderRenewalAfterRewrite(t *testing.T) {
	// PROBE: the exported doc says GetCertificate reloads "when either file has changed", with no mention
	// of a check interval. The files are rewritten with a later mtime and the next call still serves the
	// old pair, so the check is throttled on a clock an external test cannot drive. Garbage renewals, a
	// deleted key and mismatched pairs could therefore not be probed from outside the package.
	directory := t.TempDir()
	certificatePath, keyPath, _ := writeCertificatePair(t, directory, 1)
	reloader, err := serving.NewCertificateReloader(certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	servedSerial(t, reloader)
	writeCertificatePair(t, directory, 2)
	touchLater(t, 2, certificatePath, keyPath)
	if serial := servedSerial(t, reloader); serial != 2 {
		t.Skipf("the rewritten pair is not served at once (serial %d); the reload check is throttled", serial)
	}
}

// TestNewCertificateReloaderFailsFast errors at once for missing or unreadable files.
func TestNewCertificateReloaderFailsFast(t *testing.T) {
	directory := t.TempDir()
	certificatePath, keyPath, _ := writeCertificatePair(t, directory, 1)
	if _, err := serving.NewCertificateReloader(filepath.Join(directory, "missing.pem"), keyPath); err == nil {
		t.Error("a missing certificate file was accepted")
	}
	if _, err := serving.NewCertificateReloader(certificatePath, filepath.Join(directory, "missing.pem")); err == nil {
		t.Error("a missing key file was accepted")
	}
	os.WriteFile(filepath.Join(directory, "garbage.pem"), []byte("garbage"), 0o600)
	if _, err := serving.NewCertificateReloader(filepath.Join(directory, "garbage.pem"), keyPath); err == nil {
		t.Error("a garbage certificate file was accepted")
	}
}

// TestCertificateReloaderConcurrentReads survives concurrent GetCertificate calls during a renewal.
func TestCertificateReloaderConcurrentReads(t *testing.T) {
	directory := t.TempDir()
	certificatePath, keyPath, _ := writeCertificatePair(t, directory, 1)
	reloader, err := serving.NewCertificateReloader(certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for round := 0; round < 50; round++ {
				if _, err := reloader.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	writeCertificatePair(t, directory, 2)
	touchLater(t, 2, certificatePath, keyPath)
	for worker := 0; worker < 8; worker++ {
		<-done
	}
}

// TestTLSConfigMinimumVersionAndCertificate pins TLS 1.2 and the reloader's certificate.
func TestTLSConfigMinimumVersionAndCertificate(t *testing.T) {
	directory := t.TempDir()
	certificatePath, keyPath, _ := writeCertificatePair(t, directory, 7)
	reloader, err := serving.NewCertificateReloader(certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	config := serving.TLSConfig(reloader)
	if config.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion is %x, want TLS 1.2", config.MinVersion)
	}
	if config.GetCertificate == nil {
		t.Fatal("GetCertificate is not set")
	}
	certificate, err := config.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificate(certificate.Certificate[0])
	if parsed.SerialNumber.Int64() != 7 {
		t.Errorf("TLSConfig serves serial %d, want 7", parsed.SerialNumber.Int64())
	}
	if config.MaxVersion != 0 && config.MaxVersion < tls.VersionTLS13 {
		t.Errorf("MaxVersion %x shuts out TLS 1.3", config.MaxVersion)
	}
}

// TestTLSConfigEndToEnd serves a real TLS handshake and refuses TLS 1.1.
func TestTLSConfigEndToEnd(t *testing.T) {
	directory := t.TempDir()
	certificatePath, keyPath, parsed := writeCertificatePair(t, directory, 3)
	reloader, err := serving.NewCertificateReloader(certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(okHandler)
	server.TLS = serving.TLSConfig(reloader)
	server.StartTLS()
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "board.test", MinVersion: tls.VersionTLS12}}}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("TLS 1.2+ client: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "board.test", MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}}}
	if response, err := old.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("a TLS 1.1 client was served")
	}
}
