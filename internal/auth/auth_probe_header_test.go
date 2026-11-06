package auth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
)

// probeKey returns a fresh P-256 private key.
func probeKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// headerParams holds the four parameters of a header as strings.
type headerParams struct {
	// member is the member parameter's value.
	member string

	// time is the time parameter's value.
	time string

	// nonce is the nonce parameter's value.
	nonce string

	// signature is the signature parameter's value.
	signature string
}

// genuineParams signs a request with a fresh key and returns the header's parameters.
func genuineParams(t *testing.T) headerParams {
	t.Helper()
	value, err := auth.AuthorizationValue(probeKey(t), "PUT", "/api/v1/members/me", time.Unix(1792800000, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	rest := strings.TrimPrefix(value, auth.Scheme+" ")
	var params headerParams
	for _, pair := range strings.Split(rest, ",") {
		name, raw, _ := strings.Cut(strings.TrimSpace(pair), "=")
		switch name {
		case "member":
			params.member = raw
		case "time":
			params.time = raw
		case "nonce":
			params.nonce = raw
		case "signature":
			params.signature = raw
		default:
			t.Fatalf("unexpected parameter %q in %q", name, value)
		}
	}
	return params
}

// render joins the parameters in the documented order with sep between them.
func (p headerParams) render(sep string) string {
	return auth.Scheme + " member=" + p.member + sep + "time=" + p.time + sep + "nonce=" + p.nonce + sep + "signature=" + p.signature
}

// pairs returns the parameters as name=value strings in the documented order.
func (p headerParams) pairs() []string {
	return []string{"member=" + p.member, "time=" + p.time, "nonce=" + p.nonce, "signature=" + p.signature}
}

// permutations returns every ordering of items.
func permutations(items []string) [][]string {
	if len(items) <= 1 {
		return [][]string{append([]string(nil), items...)}
	}
	var out [][]string
	for i := range items {
		rest := append(append([]string(nil), items[:i]...), items[i+1:]...)
		for _, tail := range permutations(rest) {
			out = append(out, append([]string{items[i]}, tail...))
		}
	}
	return out
}

// TestParseHeaderAcceptsAnyOrder parses all 24 orderings of the parameters identically.
func TestParseHeaderAcceptsAnyOrder(t *testing.T) {
	params := genuineParams(t)
	want, err := auth.ParseHeader(params.render(","))
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range permutations(params.pairs()) {
		value := auth.Scheme + " " + strings.Join(order, ",")
		got, err := auth.ParseHeader(value)
		if err != nil {
			t.Errorf("ParseHeader(%q) = %v", value, err)
			continue
		}
		if got.Member != want.Member || got.Time != want.Time || got.Nonce != want.Nonce || string(got.Signature) != string(want.Signature) {
			t.Errorf("ParseHeader(%q) differed from the documented order", value)
		}
	}
}

// TestParseHeaderSeparators checks the separators the contract allows and refuses the rest.
func TestParseHeaderSeparators(t *testing.T) {
	params := genuineParams(t)
	cases := []struct {
		// name describes the separator.
		name string

		// sep is placed between parameters.
		sep string

		// ok is whether the contract allows it.
		ok bool

		// probe marks a separator the contract does not mention; the outcome is only logged.
		probe bool
	}{
		{name: "comma", sep: ",", ok: true},
		{name: "comma space", sep: ", ", ok: true},
		{name: "comma two spaces", sep: ",  ", ok: true},
		{name: "comma tab", sep: ",\t", probe: true},
		{name: "space comma", sep: " ,", probe: true},
		{name: "space comma space", sep: " , ", probe: true},
		{name: "semicolon", sep: ";"},
		{name: "space only", sep: " "},
		{name: "double comma", sep: ",,"},
		{name: "comma newline", sep: ",\n", probe: true},
		{name: "comma CRLF space", sep: ",\r\n ", probe: true},
		{name: "comma NBSP", sep: ",\u00a0", probe: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.ParseHeader(params.render(tc.sep))
			if tc.probe {
				t.Logf("PROBE: separator %q parsed=%v", tc.sep, err == nil)
				return
			}
			if (err == nil) != tc.ok {
				t.Errorf("separator %q: err=%v, want ok=%v", tc.sep, err, tc.ok)
			}
		})
	}
}

// TestParseHeaderFraming checks whitespace and punctuation around the parameter list.
func TestParseHeaderFraming(t *testing.T) {
	params := genuineParams(t)
	list := strings.TrimPrefix(params.render(","), auth.Scheme+" ")
	cases := []struct {
		// name describes the framing.
		name string

		// value is the whole header value.
		value string

		// probe marks a framing the contract does not mention; the outcome is only logged.
		probe bool
	}{
		{name: "trailing comma", value: auth.Scheme + " " + list + ","},
		{name: "trailing comma space", value: auth.Scheme + " " + list + ", "},
		{name: "leading comma", value: auth.Scheme + " ," + list},
		{name: "no space after scheme", value: auth.Scheme + list},
		{name: "scheme only", value: auth.Scheme},
		{name: "scheme and space", value: auth.Scheme + " "},
		{name: "other scheme same params", value: "Bearer " + list},
		{name: "scheme prefix", value: auth.Scheme + "X " + list},
		{name: "two schemes", value: auth.Scheme + " " + auth.Scheme + " " + list},
		{name: "two spaces after scheme", value: auth.Scheme + "  " + list, probe: true},
		{name: "tab after scheme", value: auth.Scheme + "\t" + list, probe: true},
		{name: "leading space", value: " " + auth.Scheme + " " + list, probe: true},
		{name: "trailing space", value: auth.Scheme + " " + list + " ", probe: true},
		{name: "trailing CRLF", value: auth.Scheme + " " + list + "\r\n", probe: true},
		{name: "lower-case scheme", value: strings.ToLower(auth.Scheme) + " " + list, probe: true},
		{name: "upper-case scheme", value: strings.ToUpper(auth.Scheme) + " " + list, probe: true},
		{name: "upper-case parameter name", value: auth.Scheme + " " + strings.Replace(list, "member=", "MEMBER=", 1), probe: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.ParseHeader(tc.value)
			if tc.probe {
				t.Logf("PROBE: %s parsed=%v", tc.name, err == nil)
				return
			}
			if err == nil {
				t.Errorf("accepted %q", tc.value)
			}
		})
	}
}

// TestParseHeaderEachParameterExactlyOnce refuses a missing, repeated or unknown parameter.
func TestParseHeaderEachParameterExactlyOnce(t *testing.T) {
	params := genuineParams(t)
	pairs := params.pairs()
	for i, pair := range pairs {
		name, _, _ := strings.Cut(pair, "=")
		t.Run("missing "+name, func(t *testing.T) {
			rest := append(append([]string(nil), pairs[:i]...), pairs[i+1:]...)
			if _, err := auth.ParseHeader(auth.Scheme + " " + strings.Join(rest, ",")); err == nil {
				t.Errorf("accepted a header without %s", name)
			}
		})
		t.Run("repeated "+name, func(t *testing.T) {
			if _, err := auth.ParseHeader(auth.Scheme + " " + strings.Join(append(pairs, pair), ",")); err == nil {
				t.Errorf("accepted a header with %s twice", name)
			}
		})
		t.Run("repeated "+name+" adjacent", func(t *testing.T) {
			doubled := append(append(append([]string(nil), pairs[:i]...), pair, pair), pairs[i+1:]...)
			if _, err := auth.ParseHeader(auth.Scheme + " " + strings.Join(doubled, ",")); err == nil {
				t.Errorf("accepted a header with %s twice in a row", name)
			}
		})
	}
	t.Run("unknown parameter", func(t *testing.T) {
		for _, extra := range []string{"extra=1", "realm=board", "member2=x", "=", "x", "signature2=" + params.signature} {
			if _, err := auth.ParseHeader(auth.Scheme + " " + strings.Join(append(pairs, extra), ",")); err == nil {
				t.Errorf("accepted an unknown parameter %q", extra)
			}
		}
	})
	t.Run("empty parameter", func(t *testing.T) {
		if _, err := auth.ParseHeader(auth.Scheme + " " + strings.Join(append(pairs, ""), ",")); err == nil {
			t.Error("accepted an empty parameter after a trailing comma")
		}
	})
}

// TestParseHeaderValueShapes checks each parameter against its documented shape.
func TestParseHeaderValueShapes(t *testing.T) {
	base := genuineParams(t)
	padded22 := strings.Repeat("A", 22) + "=="
	cases := []struct {
		// name describes the mutation.
		name string

		// mutate returns the header value to parse.
		mutate func(p headerParams) headerParams

		// ok is whether the contract accepts the value.
		ok bool

		// probe marks a shape the contract leaves open; the outcome is only logged.
		probe bool
	}{
		{name: "member 21 chars", mutate: func(p headerParams) headerParams { p.member = p.member[:21]; return p }},
		{name: "member 23 chars", mutate: func(p headerParams) headerParams { p.member += "A"; return p }},
		{name: "member padded", mutate: func(p headerParams) headerParams { p.member = padded22; return p }},
		{name: "member empty", mutate: func(p headerParams) headerParams { p.member = ""; return p }},
		{name: "member quoted", mutate: func(p headerParams) headerParams { p.member = `"` + p.member + `"`; return p }, probe: true},
		{name: "member plus", mutate: func(p headerParams) headerParams { p.member = "+" + p.member[1:]; return p }},
		{name: "member slash", mutate: func(p headerParams) headerParams { p.member = "/" + p.member[1:]; return p }},
		{name: "member dot", mutate: func(p headerParams) headerParams { p.member = "." + p.member[1:]; return p }},
		{name: "member space inside", mutate: func(p headerParams) headerParams { p.member = p.member[:10] + " " + p.member[11:]; return p }},
		{name: "member non-ASCII", mutate: func(p headerParams) headerParams { p.member = "é" + p.member[2:]; return p }},
		{name: "member non-canonical trailing bits", mutate: func(p headerParams) headerParams { p.member = p.member[:21] + "B"; return p }, probe: true},
		{name: "member canonical trailing bits", mutate: func(p headerParams) headerParams { p.member = p.member[:21] + "Q"; return p }, ok: true},
		{name: "nonce 21 chars", mutate: func(p headerParams) headerParams { p.nonce = p.nonce[:21]; return p }},
		{name: "nonce 23 chars", mutate: func(p headerParams) headerParams { p.nonce += "A"; return p }},
		{name: "nonce padded", mutate: func(p headerParams) headerParams { p.nonce = padded22; return p }},
		{name: "nonce empty", mutate: func(p headerParams) headerParams { p.nonce = ""; return p }},
		{name: "nonce plus", mutate: func(p headerParams) headerParams { p.nonce = "+" + p.nonce[1:]; return p }},
		{name: "nonce slash", mutate: func(p headerParams) headerParams { p.nonce = "/" + p.nonce[1:]; return p }},
		{name: "nonce quoted", mutate: func(p headerParams) headerParams { p.nonce = `"` + p.nonce + `"`; return p }, probe: true},
		{name: "nonce non-canonical trailing bits", mutate: func(p headerParams) headerParams { p.nonce = p.nonce[:21] + "B"; return p }, probe: true},
		{name: "nonce all underscores", mutate: func(p headerParams) headerParams { p.nonce = strings.Repeat("_", 21) + "w"; return p }, ok: true},
		{name: "nonce all hyphens", mutate: func(p headerParams) headerParams { p.nonce = strings.Repeat("-", 21) + "w"; return p }, ok: true},
		{name: "signature 85 chars", mutate: func(p headerParams) headerParams { p.signature = p.signature[:85]; return p }},
		{name: "signature 87 chars", mutate: func(p headerParams) headerParams { p.signature += "A"; return p }},
		{name: "signature 88 padded", mutate: func(p headerParams) headerParams { p.signature += "=="; return p }},
		{name: "signature 84 chars", mutate: func(p headerParams) headerParams { p.signature = p.signature[:84]; return p }},
		{name: "signature empty", mutate: func(p headerParams) headerParams { p.signature = ""; return p }},
		{name: "signature plus", mutate: func(p headerParams) headerParams { p.signature = "+" + p.signature[1:]; return p }},
		{name: "signature slash", mutate: func(p headerParams) headerParams { p.signature = "/" + p.signature[1:]; return p }},
		{name: "signature quoted", mutate: func(p headerParams) headerParams { p.signature = `"` + p.signature + `"`; return p }, probe: true},
		{name: "signature equals inside", mutate: func(p headerParams) headerParams { p.signature = p.signature[:40] + "=" + p.signature[41:]; return p }},
		{name: "signature hex", mutate: func(p headerParams) headerParams { p.signature = strings.Repeat("ab", 64); return p }},
		{name: "signature non-canonical trailing bits", mutate: func(p headerParams) headerParams { p.signature = p.signature[:85] + "B"; return p }, probe: true},
		{name: "signature canonical trailing bits", mutate: func(p headerParams) headerParams { p.signature = p.signature[:85] + "Q"; return p }, ok: true},
		{name: "signature trailing LF", mutate: func(p headerParams) headerParams { p.signature += "\n"; return p }},
		{name: "signature embedded LF", mutate: func(p headerParams) headerParams { p.signature = p.signature[:40] + "\n" + p.signature[40:]; return p }},
		{name: "signature embedded CRLF", mutate: func(p headerParams) headerParams {
			p.signature = p.signature[:40] + "\r\n" + p.signature[40:]
			return p
		}},
		{name: "member embedded LF", mutate: func(p headerParams) headerParams { p.member = p.member[:10] + "\n" + p.member[10:]; return p }},
		{name: "nonce embedded CR", mutate: func(p headerParams) headerParams { p.nonce = p.nonce[:10] + "\r" + p.nonce[10:]; return p }},
		{name: "member standard alphabet with URL-specific char", mutate: func(p headerParams) headerParams { p.member = strings.Repeat("A", 20) + "+Q"; return p }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.mutate(base).render(",")
			_, err := auth.ParseHeader(value)
			if tc.probe {
				t.Logf("PROBE: %s parsed=%v", tc.name, err == nil)
				return
			}
			if (err == nil) != tc.ok {
				t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
			}
		})
	}
}

// TestParseHeaderReturnsDocumentedShapes requires an accepted member and nonce to be 22 base64url characters.
func TestParseHeaderReturnsDocumentedShapes(t *testing.T) {
	base := genuineParams(t)
	for _, mutate := range []func(p headerParams) headerParams{
		func(p headerParams) headerParams { return p },
		func(p headerParams) headerParams { p.member = p.member[:10] + "\n" + p.member[10:]; return p },
		func(p headerParams) headerParams { p.nonce = p.nonce[:10] + "\r\n" + p.nonce[10:]; return p },
		func(p headerParams) headerParams { p.nonce = p.nonce[:10] + "\r" + p.nonce[10:]; return p },
	} {
		value := mutate(base).render(",")
		header, err := auth.ParseHeader(value)
		if err != nil {
			continue
		}
		if len(header.Member) != 22 || strings.ContainsAny(header.Member, "+/=\r\n \t") {
			t.Errorf("accepted member %q from %q", header.Member, value)
		}
		if len(header.Nonce) != 22 || strings.ContainsAny(header.Nonce, "+/=\r\n \t") {
			t.Errorf("accepted nonce %q from %q", header.Nonce, value)
		}
		if len(header.Signature) != 64 {
			t.Errorf("accepted signature of %d bytes from %q", len(header.Signature), value)
		}
	}
}

// TestParseHeaderNonCanonicalSignatureDecodesToSameBytes documents whether two spellings of one signature both parse.
func TestParseHeaderNonCanonicalSignatureDecodesToSameBytes(t *testing.T) {
	params := genuineParams(t)
	canonical, err := auth.ParseHeader(params.render(","))
	if err != nil {
		t.Fatal(err)
	}
	last := params.signature[85]
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	index := strings.IndexByte(alphabet, last)
	if index < 0 {
		t.Fatalf("signature ends in %q", last)
	}
	// The same high two bits with the low four bits set spell the same 64 bytes non-canonically.
	params.signature = params.signature[:85] + string(alphabet[(index&0x30)|0x0f])
	other, err := auth.ParseHeader(params.render(","))
	if err != nil {
		t.Logf("PROBE: non-canonical signature refused: %v", err)
		return
	}
	t.Logf("PROBE: non-canonical signature accepted; same bytes=%v", string(other.Signature) == string(canonical.Signature))
}

// TestParseHeaderTime checks the time parameter's syntax.
func TestParseHeaderTime(t *testing.T) {
	base := genuineParams(t)
	cases := []struct {
		// name describes the value.
		name string

		// value is the time parameter.
		value string

		// ok is whether the contract accepts it.
		ok bool

		// want is the parsed time when ok.
		want int64

		// probe marks a spelling the contract leaves open; the outcome is only logged.
		probe bool
	}{
		{name: "plain", value: "1792800000", ok: true, want: 1792800000},
		{name: "zero", value: "0", ok: true, want: 0},
		{name: "max int64", value: "9223372036854775807", ok: true, want: 9223372036854775807},
		{name: "overflow by one", value: "9223372036854775808"},
		{name: "huge", value: "99999999999999999999999999"},
		{name: "min int64", value: "-9223372036854775808", probe: true},
		{name: "underflow", value: "-9223372036854775809"},
		{name: "negative", value: "-1", probe: true},
		{name: "leading plus", value: "+1792800000", probe: true},
		{name: "leading zeros", value: "0001792800000", probe: true},
		{name: "negative zero", value: "-0", probe: true},
		{name: "float", value: "1792800000.0"},
		{name: "exponent", value: "1e9"},
		{name: "hex", value: "0x6ADB4D80"},
		{name: "octal prefix", value: "0o17"},
		{name: "binary prefix", value: "0b1"},
		{name: "underscores", value: "1_792_800_000"},
		{name: "empty", value: ""},
		{name: "leading space", value: " 1792800000"},
		{name: "trailing space", value: "1792800000 ", probe: true},
		{name: "trailing tab", value: "1792800000\t", probe: true},
		{name: "quoted", value: `"1792800000"`, probe: true},
		{name: "fullwidth digits", value: "１７９２８００００"},
		{name: "arabic-indic digits", value: "١٧٩٢٨٠٠٠٠٠"},
		{name: "letters", value: "soon"},
		{name: "milliseconds-looking", value: "1792800000000", ok: true, want: 1792800000000},
		{name: "RFC 3339", value: "2026-10-24T04:00:00Z"},
		{name: "nul", value: "1792800000\x00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := base
			params.time = tc.value
			header, err := auth.ParseHeader(params.render(","))
			if tc.probe {
				t.Logf("PROBE: time=%q parsed=%v time=%d", tc.value, err == nil, header.Time)
				return
			}
			if (err == nil) != tc.ok {
				t.Fatalf("time=%q: err=%v, want ok=%v", tc.value, err, tc.ok)
			}
			if tc.ok && header.Time != tc.want {
				t.Errorf("time=%q parsed as %d", tc.value, header.Time)
			}
		})
	}
}

// TestParseHeaderErrorsDoNotEchoValues keeps a hostile value out of the error text.
func TestParseHeaderErrorsDoNotEchoValues(t *testing.T) {
	base := genuineParams(t)
	marker := "ECHOED\r\nX-Injected: 1"
	for _, mutate := range []func(p headerParams) headerParams{
		func(p headerParams) headerParams { p.member = marker; return p },
		func(p headerParams) headerParams { p.time = marker; return p },
		func(p headerParams) headerParams { p.nonce = marker; return p },
		func(p headerParams) headerParams { p.signature = marker; return p },
	} {
		value := mutate(base).render(",")
		_, err := auth.ParseHeader(value)
		if err == nil {
			t.Errorf("accepted %q", value)
			continue
		}
		if strings.Contains(err.Error(), "ECHOED") || strings.Contains(err.Error(), "\r") {
			t.Errorf("error echoed the value: %q", err.Error())
		}
	}
}

// TestParseHeaderHugeInput refuses a very long header without taking long.
func TestParseHeaderHugeInput(t *testing.T) {
	base := genuineParams(t)
	start := time.Now()
	for _, value := range []string{
		auth.Scheme + " " + strings.Repeat("member=x,", 100000) + strings.TrimPrefix(base.render(","), auth.Scheme+" "),
		auth.Scheme + " " + strings.Repeat(",", 1<<20),
		auth.Scheme + " member=" + strings.Repeat("A", 1<<20),
		auth.Scheme + " " + strings.Repeat(" ", 1<<20),
	} {
		if _, err := auth.ParseHeader(value); err == nil {
			t.Error("accepted a huge malformed header")
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("parsing huge headers took %v", elapsed)
	}
}

// FuzzParseHeader checks ParseHeader never panics and that an accepted header has the documented shape.
func FuzzParseHeader(f *testing.F) {
	member := strings.Repeat("A", 22)
	signature := strings.Repeat("A", 86)
	f.Add(auth.Scheme + " member=" + member + ",time=1,nonce=" + member + ",signature=" + signature)
	f.Add(auth.Scheme + " member=" + member + ", time=1, nonce=" + member + ", signature=" + signature)
	f.Add(auth.Scheme + " signature=" + signature + ",nonce=" + member + ",time=-5,member=" + member)
	f.Add(auth.Scheme + " member=" + member + ",time=1,nonce=" + member + ",signature=" + signature + ",")
	f.Add(auth.Scheme + "  member=,time=,nonce=,signature=")
	f.Add(auth.Scheme)
	f.Add("")
	f.Add("Bearer x")
	f.Add(auth.Scheme + " member=" + member + ",time=99999999999999999999,nonce=" + member + ",signature=" + signature)
	f.Add(auth.Scheme + " member=\"" + member + "\",time=1,nonce=" + member + ",signature=" + signature)
	f.Fuzz(func(t *testing.T, value string) {
		header, err := auth.ParseHeader(value)
		if err != nil {
			return
		}
		if strings.ContainsAny(value, "\r\n") {
			// Known: CR and LF inside a value are accepted; TestParseHeaderValueShapes covers it.
			return
		}
		if len(header.Member) != 22 {
			t.Errorf("accepted member of %d chars from %q", len(header.Member), value)
		}
		if len(header.Nonce) != 22 {
			t.Errorf("accepted nonce of %d chars from %q", len(header.Nonce), value)
		}
		if len(header.Signature) != 64 {
			t.Errorf("accepted signature of %d bytes from %q", len(header.Signature), value)
		}
		if strings.ContainsAny(header.Member+header.Nonce, "+/= \t\r\n\"") {
			t.Errorf("accepted non-base64url characters from %q", value)
		}
		if !strings.HasPrefix(value, auth.Scheme) {
			t.Errorf("accepted a header without the scheme: %q", value)
		}
	})
}
