// Package serving is how a board meets the network: TLS from certificate files it reloads when they are
// renewed, or plain HTTP behind a proxy that terminates TLS, and nothing in plain text otherwise.
package serving

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// The modes a board serves in.
const (
	// ModeDevelopment serves plain HTTP on loopback only.
	ModeDevelopment = "development"

	// ModeFiles serves TLS from certificate files.
	ModeFiles = "files"

	// ModeProxy serves plain HTTP behind a proxy that terminates TLS.
	ModeProxy = "proxy"
)

// reloadCheckInterval is how often a handshake may look at the certificate files for a renewal.
const reloadCheckInterval = 30 * time.Second

// TLSConfig returns the server's TLS: version 1.2 at least, Go's default suites, which for 1.2 are all
// ECDHE with AEAD preferred over AES-CBC, and the certificate from reloader.
func TLSConfig(reloader *CertificateReloader) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: reloader.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

// CertificateReloader serves a certificate from files and picks up a renewal without a restart.
type CertificateReloader struct {
	// certificatePath is the PEM certificate chain's file.
	certificatePath string

	// keyPath is the PEM private key's file.
	keyPath string

	// now is the clock, replaced in tests.
	now func() time.Time

	// mutex guards every field below it.
	mutex sync.Mutex

	// certificate is the certificate being served.
	certificate *tls.Certificate

	// modified is the later of the two files' modification times when certificate was loaded.
	modified time.Time

	// checked is when the files were last looked at.
	checked time.Time
}

// NewCertificateReloader loads a certificate and its key, failing at once if they cannot be read.
func NewCertificateReloader(certificatePath, keyPath string) (*CertificateReloader, error) {
	reloader := &CertificateReloader{certificatePath: certificatePath, keyPath: keyPath, now: time.Now}
	if err := reloader.load(); err != nil {
		return nil, err
	}
	return reloader, nil
}

// GetCertificate returns the current certificate, reloading it when either file has changed.
func (r *CertificateReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	now := r.now()
	if now.Sub(r.checked) >= reloadCheckInterval {
		r.checked = now
		if modified, err := latestModification(r.certificatePath, r.keyPath); err == nil && modified.After(r.modified) {
			// A renewal half written keeps the old certificate until the next check.
			if certificate, err := tls.LoadX509KeyPair(r.certificatePath, r.keyPath); err == nil {
				r.certificate, r.modified = &certificate, modified
			}
		}
	}
	return r.certificate, nil
}

// load reads the certificate and key, failing if either cannot be read.
func (r *CertificateReloader) load() error {
	modified, err := latestModification(r.certificatePath, r.keyPath)
	if err != nil {
		return err
	}
	certificate, err := tls.LoadX509KeyPair(r.certificatePath, r.keyPath)
	if err != nil {
		return fmt.Errorf("serving: loading the certificate: %w", err)
	}
	r.certificate, r.modified, r.checked = &certificate, modified, r.now()
	return nil
}

// latestModification returns the latest modification time of paths.
func latestModification(paths ...string) (time.Time, error) {
	var latest time.Time
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return time.Time{}, fmt.Errorf("serving: %w", err)
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest, nil
}

// RequireProxyHTTPS refuses any request the proxy did not receive over HTTPS, so a board behind a proxy
// never answers plain text that came around it.
func RequireProxyHTTPS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.EqualFold(strings.TrimSpace(forwardedHeader(request, "X-Forwarded-Proto")), "https") {
			http.Error(writer, "This board only answers over HTTPS.", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// forwardedHeader joins every line of a repeated header into the one list RFC 9110 defines, since a proxy may add
// its own line rather than append to the client's.
func forwardedHeader(request *http.Request, name string) string {
	return strings.Join(request.Header.Values(name), ",")
}

// ProxyClientAddress reads the address the proxy saw, the last entry of X-Forwarded-For, which is the
// one the proxy itself added.
func ProxyClientAddress(request *http.Request) string {
	if forwarded := forwardedHeader(request, "X-Forwarded-For"); forwarded != "" {
		entries := strings.Split(forwarded, ",")
		if address := strings.TrimSpace(entries[len(entries)-1]); address != "" {
			return address
		}
	}
	return peerAddress(request)
}

// peerAddress returns the address of the connection's peer, without its port.
func peerAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

// TrustedProxies is the set of peers whose X-Forwarded-* headers a board in proxy mode believes; empty
// believes every peer.
type TrustedProxies []netip.Prefix

// TrustAnyProxy is the trusted-proxies value that believes every peer, the behaviour before a default existed.
const TrustAnyProxy = "any"

// NonPublicProxies are the ranges a board on every interface trusts by default: loopback, private, shared
// (Container Apps) and link-local (Cloud Run), none of which a peer on the internet can come from.
func NonPublicProxies() TrustedProxies {
	trusted, _ := ParseTrustedProxies("127.0.0.0/8 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 " +
		"169.254.0.0/16 ::1/128 fc00::/7 fe80::/10")
	return trusted
}

// ParseTrustedProxies reads a list of CIDRs or single addresses separated by commas or spaces.
func ParseTrustedProxies(value string) (TrustedProxies, error) {
	var result TrustedProxies
	for _, entry := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if !strings.Contains(entry, "/") {
			address, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, fmt.Errorf("serving: the trusted proxy %q is not an address or CIDR", entry)
			}
			address = address.Unmap().WithZone("")
			result = append(result, netip.PrefixFrom(address, address.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("serving: the trusted proxy %q is not an address or CIDR", entry)
		}
		result = append(result, prefix.Masked())
	}
	return result, nil
}

// Contains reports whether address, with or without a port, is in one of the trusted ranges.
func (t TrustedProxies) Contains(address string) bool {
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	parsed, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	parsed = parsed.Unmap().WithZone("")
	for _, prefix := range t {
		if prefix.Contains(parsed) {
			return true
		}
	}
	return false
}

// Require refuses with 421 a request from a peer outside the set, and passes everything when it is empty.
func (t TrustedProxies) Require(next http.Handler) http.Handler {
	if len(t) == 0 {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !t.Contains(request.RemoteAddr) {
			http.Error(writer, "This board only answers through its proxy.", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// ClientAddress is ProxyClientAddress when the set is empty. Otherwise it ignores X-Forwarded-For from an
// untrusted peer, and from a trusted one returns the last entry not itself a trusted proxy.
func (t TrustedProxies) ClientAddress(request *http.Request) string {
	if len(t) == 0 {
		return ProxyClientAddress(request)
	}
	peer := peerAddress(request)
	if !t.Contains(peer) {
		return peer
	}
	entries := strings.Split(forwardedHeader(request, "X-Forwarded-For"), ",")
	for index := len(entries) - 1; index >= 0; index-- {
		entry := strings.TrimSpace(entries[index])
		if entry == "" {
			continue
		}
		if !t.Contains(entry) || index == 0 {
			return entry
		}
	}
	return peer
}

// CheckListenAddress refuses an address the mode should not listen on. Development serves plain HTTP,
// so only loopback; behind a proxy, loopback, a private network, or every interface inside a container,
// never one public address.
func CheckListenAddress(mode, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("serving: the listen address: %w", err)
	}
	ip := net.ParseIP(host)
	isLoopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	switch mode {
	case ModeDevelopment:
		if !isLoopback {
			return fmt.Errorf("serving: development mode serves plain HTTP, so it listens on loopback only, not %s", address)
		}
	case ModeProxy:
		if host == "" || isLoopback || (ip != nil && (ip.IsPrivate() || ip.IsUnspecified())) {
			return nil
		}
		return fmt.Errorf("serving: behind a proxy the board serves plain HTTP, so it never listens on the public address %s", address)
	case ModeFiles:
	default:
		return errors.New("serving: the mode is development, files or proxy")
	}
	return nil
}
