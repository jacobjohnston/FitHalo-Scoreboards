package store

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// dictionary1Digest is dictionary1's SHA-256, pinned so the dictionary cannot change unnoticed.
const dictionary1Digest = "d3e287e4fca4ab830a43f604602b6e8b9a909a43f0da0e26746776dc1807c657"

// sampleChallenge is a weekly challenge record of the shape boards store most.
const sampleChallenge = `{"progress":0.84,"score":420,"goal":500,"tier":"monster","breakdown":{"exercise":380,` +
	`"dailyBonus":30,"varietyBonus":10},"activities":[{"id":"Q2xvdWRTdG9yZVJlY29yZA","type":"outdoorCycling",` +
	`"day":"2026-10-20","points":{"base":42,"bonus":3},"durationSeconds":5400,"distanceMeters":48210.5,` +
	`"start":"2026-10-20T07:12:00Z","details":{"activeEnergyKilocalories":910.2,"averageHeartRate":141.3,` +
	`"maximumHeartRate":176,"averageSpeedMetersPerSecond":8.93,"elevationGainMeters":402.1},"hasRoute":true}],` +
	`"updatedAt":"2026-10-25T09:00:00Z"}`

// TestCompressed holds the compressing wrapper to the store's contract over every backend.
func TestCompressed(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) Store{
		"directory": openTestDirectory, "dynamodb": openFakeDynamoDB, "cosmos": openFakeCosmos,
	} {
		t.Run(name, func(t *testing.T) {
			runContract(t, func(t *testing.T) Store { return Compress(open(t)) })
		})
	}
}

// TestStoresTheSmallerForm proves a weekly challenge is kept compressed and random bytes are kept as written.
func TestStoresTheSmallerForm(t *testing.T) {
	ctx := context.Background()
	backend := openTestDirectory(t)
	compressed := Compress(backend)
	random := make([]byte, 4096)
	rand.Read(random)

	for _, test := range []struct {
		name   string
		data   []byte
		format byte
	}{
		{"challenge", []byte(sampleChallenge), formatDeflate1},
		{"random", random, formatPlain},
		{"empty", []byte{}, formatPlain},
	} {
		if _, err := compressed.Put(ctx, "weeks", test.name, test.data, AnyVersion); err != nil {
			t.Fatal(err)
		}
		stored, err := backend.Get(ctx, "weeks", test.name)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Data[0] != test.format || len(stored.Data) > len(test.data)+1 {
			t.Errorf("%s: got format %d in %d bytes, want format %d in at most %d", test.name, stored.Data[0],
				len(stored.Data), test.format, len(test.data)+1)
		}
		read, err := compressed.Get(ctx, "weeks", test.name)
		if err != nil || !bytes.Equal(read.Data, test.data) {
			t.Errorf("%s: Get() = %d bytes, %v; want the %d bytes written", test.name, len(read.Data), err, len(test.data))
		}
	}
}

// TestDictionaryCompressesASmallChallenge proves the dictionary does better than DEFLATE alone on a small weekly challenge.
func TestDictionaryCompressesASmallChallenge(t *testing.T) {
	packed, err := pack([]byte(sampleChallenge))
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	writer, _ := flate.NewWriter(&plain, flate.BestCompression)
	writer.Write([]byte(sampleChallenge))
	writer.Close()
	if len(packed) >= plain.Len() {
		t.Fatalf("got %d bytes with the dictionary, want fewer than DEFLATE's %d", len(packed), plain.Len())
	}
}

// TestDictionaryNeverChanges pins dictionary1, which every compressed document depends on.
func TestDictionaryNeverChanges(t *testing.T) {
	digest := sha256.Sum256(dictionary1)
	if got := hex.EncodeToString(digest[:]); got != dictionary1Digest {
		t.Fatalf("got dictionary digest %s, want %s: add a new dictionary and format instead", got, dictionary1Digest)
	}
}

// TestRefusesMalformedDocuments proves a document with no format, an unknown one, bad DEFLATE data or
// too much once expanded is an error rather than data.
func TestRefusesMalformedDocuments(t *testing.T) {
	var bomb bytes.Buffer
	bomb.WriteByte(formatDeflate1)
	writer, _ := flate.NewWriterDict(&bomb, flate.BestCompression, dictionary1)
	writer.Write([]byte(strings.Repeat("x", maximumExpandedBytes+1)))
	writer.Close()

	for name, packed := range map[string][]byte{
		"empty":    {},
		"unknown":  {9, '{', '}'},
		"corrupt":  {formatDeflate1, 0xff, 0xff, 0xff},
		"too long": bomb.Bytes(),
	} {
		if _, err := expand(packed); !errors.Is(err, errMalformedDocument) && err == nil {
			t.Errorf("%s: expand() succeeded, want an error", name)
		}
	}
}

// FuzzExpand proves no stored bytes make expand panic, and that whatever pack writes expands back.
func FuzzExpand(f *testing.F) {
	f.Add([]byte(sampleChallenge))
	f.Add([]byte{formatDeflate1, 0x01})
	f.Fuzz(func(t *testing.T, data []byte) {
		expand(data)
		packed, err := pack(data)
		if err != nil {
			t.Fatal(err)
		}
		expanded, err := expand(packed)
		if err != nil || !bytes.Equal(expanded, data) {
			t.Fatalf("expand(pack(%q)) = %q, %v", data, expanded, err)
		}
	})
}
