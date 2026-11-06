package password

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// BreachRangeURL is Have I Been Pwned's range API, which answers a SHA-1 prefix with breached suffixes.
const BreachRangeURL = "https://api.pwnedpasswords.com/range/"

const (
	// breachPrefixLength is how many hex characters of the SHA-1 are sent.
	breachPrefixLength = 5

	// breachTimeout bounds a check made with the default client.
	breachTimeout = 10 * time.Second
)

// BreachChecker asks Have I Been Pwned whether a password has been breached. Only the SHA-1 prefix
// leaves the server, and the answer is padded so its size does not give the prefix away.
type BreachChecker struct {
	// Client sends the request; nil uses a client with breachTimeout.
	Client *http.Client

	// BaseURL is the range API to ask; empty uses BreachRangeURL.
	BaseURL string
}

// Breached reports whether password is in Have I Been Pwned's list of breached passwords.
func (c BreachChecker) Breached(ctx context.Context, password string) (bool, error) {
	digest := sha1.Sum([]byte(password))
	hash := strings.ToUpper(hex.EncodeToString(digest[:]))
	prefix, suffix := hash[:breachPrefixLength], hash[breachPrefixLength:]

	baseURL, client := c.BaseURL, c.Client
	if baseURL == "" {
		baseURL = BreachRangeURL
	}
	if client == nil {
		client = &http.Client{Timeout: breachTimeout}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+prefix, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Add-Padding", "true")
	request.Header.Set("User-Agent", "FitHalo-Scoreboard")
	response, err := client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("password: the breach check answered %d", response.StatusCode)
	}

	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		candidate, count, found := strings.Cut(strings.TrimSpace(scanner.Text()), ":")
		// Padding lines carry a count of zero.
		if found && count != "0" && strings.EqualFold(candidate, suffix) {
			return true, nil
		}
	}
	return false, scanner.Err()
}
