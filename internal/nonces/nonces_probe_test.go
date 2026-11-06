package nonces_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/nonces"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// start is the clock every probe begins at.
var start = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// openStore opens a directory store under a fresh temporary directory, returning both.
func openStore(t *testing.T) (*nonces.Store, string) {
	t.Helper()
	root := t.TempDir()
	directory, err := store.OpenDirectory(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	return &nonces.Store{Store: directory}, root
}

// TestUseIsOncePerMemberAndNonce accepts a nonce once per member.
func TestUseIsOncePerMemberAndNonce(t *testing.T) {
	nonceStore, _ := openStore(t)
	if !nonceStore.Use("alice", "n1", start) {
		t.Fatal("the first use was refused")
	}
	if nonceStore.Use("alice", "n1", start) {
		t.Fatal("the second use was accepted")
	}
	if nonceStore.Use("alice", "n1", start.Add(time.Second)) {
		t.Fatal("a later second use was accepted")
	}
	if !nonceStore.Use("bob", "n1", start) {
		t.Fatal("another member's same nonce was refused")
	}
	if !nonceStore.Use("alice", "n2", start) {
		t.Fatal("the same member's other nonce was refused")
	}
}

// TestSweepBoundary keeps a nonce through its lifetime and frees it after.
func TestSweepBoundary(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		// name names the case.
		name string

		// after is how long after the use Sweep runs.
		after time.Duration

		// freed says whether the nonce must be usable again after the sweep.
		freed bool
	}{
		{"at once", 0, false},
		{"one second before", auth.NonceLifetime - time.Second, false},
		{"one second after", auth.NonceLifetime + time.Second, true},
		{"an hour after", time.Hour, true},
	}
	for _, c := range cases {
		nonceStore, _ := openStore(t)
		if !nonceStore.Use("alice", "n1", start) {
			t.Fatalf("%s: the first use was refused", c.name)
		}
		if err := nonceStore.Sweep(ctx, start.Add(c.after)); err != nil {
			t.Fatalf("%s: Sweep: %v", c.name, err)
		}
		if got := nonceStore.Use("alice", "n1", start.Add(c.after)); got != c.freed {
			t.Errorf("%s: after a sweep %v later the nonce is usable again: %v, want %v", c.name, c.after, got, c.freed)
		}
	}
}

// TestSweepExactlyAtLifetime records whether a nonce exactly NonceLifetime old is swept.
func TestSweepExactlyAtLifetime(t *testing.T) {
	// PROBE: "older than their lifetime" reads as strictly older; the README says
	// "in the last 600 seconds". Either way the skew window (300 s) is well inside.
	nonceStore, _ := openStore(t)
	nonceStore.Use("alice", "n1", start)
	nonceStore.Sweep(context.Background(), start.Add(auth.NonceLifetime))
	t.Logf("a nonce exactly %v old is usable again after a sweep: %v", auth.NonceLifetime, nonceStore.Use("alice", "n1", start.Add(auth.NonceLifetime)))
}

// TestSweepRemovesExactlyTheExpired sweeps old nonces and keeps fresh ones in the same pass.
func TestSweepRemovesExactlyTheExpired(t *testing.T) {
	nonceStore, _ := openStore(t)
	nonceStore.Use("alice", "old", start)
	nonceStore.Use("alice", "fresh", start.Add(5*time.Minute))
	nonceStore.Use("bob", "old", start)
	if err := nonceStore.Sweep(context.Background(), start.Add(auth.NonceLifetime+time.Second)); err != nil {
		t.Fatal(err)
	}
	later := start.Add(auth.NonceLifetime + time.Second)
	if !nonceStore.Use("alice", "old", later) || !nonceStore.Use("bob", "old", later) {
		t.Error("an expired nonce was not swept")
	}
	if nonceStore.Use("alice", "fresh", later) {
		t.Error("a fresh nonce was swept")
	}
}

// TestSweepWithClockBehind leaves records newer than now alone.
func TestSweepWithClockBehind(t *testing.T) {
	nonceStore, _ := openStore(t)
	nonceStore.Use("alice", "n1", start)
	if err := nonceStore.Sweep(context.Background(), start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if nonceStore.Use("alice", "n1", start) {
		t.Error("a sweep with the clock behind removed a live nonce")
	}
}

// TestSweepEmptyStore returns nil on a store with nothing in it.
func TestSweepEmptyStore(t *testing.T) {
	nonceStore, _ := openStore(t)
	if err := nonceStore.Sweep(context.Background(), start); err != nil {
		t.Errorf("Sweep on an empty store: %v", err)
	}
	if err := nonceStore.Sweep(context.Background(), start); err != nil {
		t.Errorf("a second Sweep on an empty store: %v", err)
	}
}

// TestSweepCancelledContext records what Sweep does with an ended context.
func TestSweepCancelledContext(t *testing.T) {
	nonceStore, _ := openStore(t)
	nonceStore.Use("alice", "n1", start)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := nonceStore.Sweep(ctx, start.Add(time.Hour))
	t.Logf("Sweep with a cancelled context: %v", err)
}

// TestUseConcurrentSameNonce lets exactly one of many racing uses through.
func TestUseConcurrentSameNonce(t *testing.T) {
	nonceStore, _ := openStore(t)
	var group sync.WaitGroup
	var mutex sync.Mutex
	accepted := 0
	for worker := 0; worker < 50; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if nonceStore.Use("alice", "raced", start) {
				mutex.Lock()
				accepted++
				mutex.Unlock()
			}
		}()
	}
	group.Wait()
	if accepted != 1 {
		t.Errorf("%d of 50 racing uses were accepted, want exactly 1", accepted)
	}
}

// TestUseConcurrentDistinctNonces accepts every distinct nonce under contention.
func TestUseConcurrentDistinctNonces(t *testing.T) {
	nonceStore, _ := openStore(t)
	var group sync.WaitGroup
	results := make([]bool, 40)
	for worker := range results {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			results[worker] = nonceStore.Use("alice", "nonce"+strings.Repeat("x", worker%5)+string(rune('A'+worker%26))+string(rune('a'+worker/26)), start)
		}(worker)
	}
	group.Wait()
	for worker, ok := range results {
		if !ok {
			t.Errorf("distinct nonce %d was refused", worker)
		}
	}
}

// failingStore is a store whose reads and writes fail with its error.
type failingStore struct {
	// err is returned from Get and Put; nil lets the call succeed as an empty store.
	err error

	// putErr overrides err for Put when set.
	putErr error
}

// Get returns the configured error, or ErrNotFound.
func (f *failingStore) Get(context.Context, string, string) (store.Document, error) {
	if f.err != nil {
		return store.Document{}, f.err
	}
	return store.Document{}, store.ErrNotFound
}

// Put returns the configured error.
func (f *failingStore) Put(context.Context, string, string, []byte, int64) (int64, error) {
	if f.putErr != nil {
		return 0, f.putErr
	}
	if f.err != nil {
		return 0, f.err
	}
	return 1, nil
}

// Delete fails.
func (f *failingStore) Delete(context.Context, string, string, int64) error { return f.err }

// List fails.
func (f *failingStore) List(context.Context, string) ([]store.Item, error) { return nil, f.err }

// Collections fails.
func (f *failingStore) Collections(context.Context, string) ([]string, error) { return nil, f.err }

// DeleteCollection fails.
func (f *failingStore) DeleteCollection(context.Context, string) error { return f.err }

// Close does nothing.
func (f *failingStore) Close() error { return nil }

// TestUseFailsClosed refuses when the store errors or reports a conflict.
func TestUseFailsClosed(t *testing.T) {
	cases := map[string]*failingStore{
		"io error":         {err: errors.New("disk on fire")},
		"conflict on put":  {putErr: store.ErrConflict},
		"not found on put": {putErr: store.ErrNotFound},
		"context deadline": {err: context.DeadlineExceeded},
		"wrapped conflict": {putErr: errors.Join(errors.New("dynamodb"), store.ErrConflict)},
	}
	for name, backend := range cases {
		if (&nonces.Store{Store: backend}).Use("alice", "n1", start) {
			t.Errorf("%s: Use accepted a nonce the store could not record", name)
		}
	}
	if !(&nonces.Store{Store: &failingStore{}}).Use("alice", "n1", start) {
		t.Error("a healthy store refused the first use")
	}
}

// TestSweepReportsStoreErrors returns the store's error from Sweep.
func TestSweepReportsStoreErrors(t *testing.T) {
	err := (&nonces.Store{Store: &failingStore{err: errors.New("disk on fire")}}).Sweep(context.Background(), start)
	// PROBE: the doc does not say whether Sweep swallows errors; the hourly sweep's caller should see them.
	t.Logf("Sweep over a failing store returned %v", err)
}

// hostileStrings are member and nonce values outside the store's identifier alphabet.
var hostileStrings = []string{"../x", "../../escape", "a/b", "", ".", "..", "+/=", "a b", "nonce\x00", strings.Repeat("a", 65), strings.Repeat("a", 64), "ünïcödé", "a\nb", "/abs", "C:\\x", "%2e%2e", "-", "_", "n.json", ".hidden", "*"}

// TestHostileNoncesNeverEscapeOrPanic keeps odd nonces inside the store and never panics.
func TestHostileNoncesNeverEscapeOrPanic(t *testing.T) {
	nonceStore, root := openStore(t)
	for _, nonce := range hostileStrings {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("nonce %q panicked: %v", nonce, recovered)
				}
			}()
			first := nonceStore.Use("alice", nonce, start)
			second := nonceStore.Use("alice", nonce, start)
			if first && second {
				t.Errorf("nonce %q was accepted twice", nonce)
			}
			t.Logf("nonce %q: first %v, second %v", nonce, first, second)
		}()
	}
	assertNothingEscaped(t, root)
}

// TestHostileMembersNeverEscapeOrPanic keeps odd member identifiers inside the store and never panics.
func TestHostileMembersNeverEscapeOrPanic(t *testing.T) {
	nonceStore, root := openStore(t)
	for _, member := range hostileStrings {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("member %q panicked: %v", member, recovered)
				}
			}()
			first := nonceStore.Use(member, "n1", start)
			second := nonceStore.Use(member, "n1", start)
			if first && second {
				t.Errorf("member %q was accepted twice", member)
			}
		}()
	}
	if err := nonceStore.Sweep(context.Background(), start.Add(time.Hour)); err != nil {
		t.Logf("Sweep after hostile members: %v", err)
	}
	assertNothingEscaped(t, root)
}

// TestHostileValuesDoNotCollide keeps distinct odd values from aliasing one another after encoding.
func TestHostileValuesDoNotCollide(t *testing.T) {
	// PROBE: if odd values are encoded into the store's alphabet, two distinct nonces must not map to one.
	nonceStore, _ := openStore(t)
	pairs := [][2]string{{"a/b", "a_b"}, {"a+b", "a-b"}, {"c d", "c_d"}, {"A", "a"}, {"n1", "n1="}, {"x", "x/"}, {"../p", "p"}, {"q/", "q"}}
	for _, pair := range pairs {
		first := nonceStore.Use("carol", pair[0], start)
		second := nonceStore.Use("carol", pair[1], start)
		if first && !second {
			t.Logf("nonces %q and %q alias (or the second was refused as invalid)", pair[0], pair[1])
		}
	}
	if nonceStore.Use("dave", "n1", start) && !nonceStore.Use("dave/x", "n1", start) {
		t.Log("members dave and dave/x alias (or the second was refused as invalid)")
	}
}

// assertNothingEscaped fails if anything but the data directory exists under root.
func assertNothingEscaped(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "data" {
			t.Errorf("%q escaped the store directory", entry.Name())
		}
	}
	parent := filepath.Dir(root)
	if _, err := os.Stat(filepath.Join(parent, "x")); err == nil {
		t.Errorf("a file escaped to %s", filepath.Join(parent, "x"))
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); err == nil {
		t.Errorf("a file escaped to %s", filepath.Join(parent, "escape"))
	}
}

// TestRealNonceShapeIsAccepted accepts the 22-character base64url nonces and member IDs the protocol uses.
func TestRealNonceShapeIsAccepted(t *testing.T) {
	nonceStore, _ := openStore(t)
	member := "Ab-_cdefghijklmnopqrst"
	nonce := "zyx-_WVUTSRQPONMLKJIHG"
	if !nonceStore.Use(member, nonce, start) {
		t.Fatal("a protocol-shaped nonce was refused")
	}
	if nonceStore.Use(member, nonce, start) {
		t.Fatal("a protocol-shaped nonce was accepted twice")
	}
	if !nonceStore.Use(member, strings.ToLower(nonce), start) {
		t.Fatal("a nonce differing only in case was refused: the store folds case")
	}
}

// TestSharedBetweenInstances refuses in one instance a nonce another accepted.
func TestSharedBetweenInstances(t *testing.T) {
	nonceStore, root := openStore(t)
	other := &nonces.Store{Store: nonceStore.Store}
	_ = root
	if !nonceStore.Use("alice", "n1", start) {
		t.Fatal("the first instance refused")
	}
	if other.Use("alice", "n1", start) {
		t.Fatal("the second instance accepted a replay")
	}
}
