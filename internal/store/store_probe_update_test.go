package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
)

// errChangeRefused is the sentinel a change function returns to abort an Update.
var errChangeRefused = errors.New("probe: change refused")

// increment parses data as a decimal counter, missing meaning zero, and returns it plus one.
func increment(data []byte) ([]byte, error) {
	var n int
	if len(data) > 0 {
		var err error
		if n, err = strconv.Atoi(string(data)); err != nil {
			return nil, err
		}
	}
	return []byte(strconv.Itoa(n + 1)), nil
}

// TestUpdateCreatesMissingDocument checks change sees nil and the result is created at version 1.
func TestUpdateCreatesMissingDocument(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		var seen []byte
		sawNil := false
		err := store.Update(context.Background(), s, "counters", "c", func(data []byte) ([]byte, error) {
			seen, sawNil = data, data == nil
			return []byte("created"), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !sawNil {
			t.Errorf("change received %q for a missing document, want nil", seen)
		}
		if document := mustGet(t, s, "counters", "c"); string(document.Data) != "created" || document.Version != 1 {
			t.Fatalf("after Update: %q at v%d", document.Data, document.Version)
		}
	})
}

// TestUpdateAppliesChangeToExisting checks change sees the current data and the version advances by one.
func TestUpdateAppliesChangeToExisting(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "counters", "c", []byte("41"), 0)
		err := store.Update(context.Background(), s, "counters", "c", func(data []byte) ([]byte, error) {
			if string(data) != "41" {
				t.Errorf("change received %q, want 41", data)
			}
			return increment(data)
		})
		if err != nil {
			t.Fatal(err)
		}
		if document := mustGet(t, s, "counters", "c"); string(document.Data) != "42" || document.Version != 2 {
			t.Fatalf("after Update: %q at v%d", document.Data, document.Version)
		}
	})
}

// TestUpdateErrorAbortsWithoutWriting checks a refusing change leaves the store untouched and surfaces the error.
func TestUpdateErrorAbortsWithoutWriting(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		err := store.Update(context.Background(), s, "counters", "missing", func([]byte) ([]byte, error) {
			return []byte("must not be written"), errChangeRefused
		})
		if !errors.Is(err, errChangeRefused) {
			t.Fatalf("Update returned %v, want the change's error", err)
		}
		if _, err := s.Get(context.Background(), "counters", "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a refused change created the document: %v", err)
		}
		mustPut(t, s, "counters", "present", []byte("keep"), 0)
		err = store.Update(context.Background(), s, "counters", "present", func(data []byte) ([]byte, error) {
			copy(data, "XXXX")
			return []byte("must not be written"), errChangeRefused
		})
		if !errors.Is(err, errChangeRefused) {
			t.Fatalf("Update returned %v, want the change's error", err)
		}
		if document := mustGet(t, s, "counters", "present"); string(document.Data) != "keep" || document.Version != 1 {
			t.Fatalf("a refused change altered the document: %q at v%d", document.Data, document.Version)
		}
	})
}

// TestUpdateChangeReturningNil records what a nil result does to the document.
func TestUpdateChangeReturningNil(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "counters", "c", []byte("value"), 0)
		// PROBE: the contract does not say whether a nil result deletes, empties or is an error.
		err := store.Update(context.Background(), s, "counters", "c", func([]byte) ([]byte, error) { return nil, nil })
		document, getErr := s.Get(context.Background(), "counters", "c")
		t.Logf("Update with nil result: %v; Get afterwards: %q v%d, %v", err, document.Data, document.Version, getErr)
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			t.Fatalf("Get after nil result: %v", getErr)
		}
		if getErr == nil && len(document.Data) != 0 && err == nil {
			t.Fatalf("Update succeeded with nil result but the document still holds %q", document.Data)
		}
	})
}

// TestUpdateRetriesAfterConflict checks a write that lands between read and write is folded in, not lost.
func TestUpdateRetriesAfterConflict(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "counters", "c", []byte("10"), 0)
		calls := 0
		err := store.Update(context.Background(), s, "counters", "c", func(data []byte) ([]byte, error) {
			calls++
			if calls == 1 {
				if _, err := s.Put(context.Background(), "counters", "c", []byte("20"), 1); err != nil {
					t.Fatalf("interfering write: %v", err)
				}
			}
			return increment(data)
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls < 2 {
			t.Fatalf("change was called %d times, want a retry after the conflict", calls)
		}
		if document := mustGet(t, s, "counters", "c"); string(document.Data) != "21" || document.Version != 3 {
			t.Fatalf("after Update: %q at v%d, want 21 at v3", document.Data, document.Version)
		}
	})
}

// TestUpdateRetriesAfterConcurrentCreate checks a create racing an Update on a missing document is folded in.
func TestUpdateRetriesAfterConcurrentCreate(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		calls := 0
		err := store.Update(context.Background(), s, "counters", "c", func(data []byte) ([]byte, error) {
			calls++
			if calls == 1 {
				if _, err := s.Put(context.Background(), "counters", "c", []byte("5"), 0); err != nil {
					t.Fatalf("interfering create: %v", err)
				}
			}
			return increment(data)
		})
		if err != nil {
			t.Fatal(err)
		}
		if document := mustGet(t, s, "counters", "c"); string(document.Data) != "6" {
			t.Fatalf("after Update: %q at v%d, want 6 (calls %d)", document.Data, document.Version, calls)
		}
	})
}

// TestUpdateRetriesAfterConcurrentDelete checks a delete racing an Update ends with the change applied to nil.
func TestUpdateRetriesAfterConcurrentDelete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "counters", "c", []byte("10"), 0)
		calls := 0
		err := store.Update(context.Background(), s, "counters", "c", func(data []byte) ([]byte, error) {
			calls++
			if calls == 1 {
				if err := s.Delete(context.Background(), "counters", "c", store.AnyVersion); err != nil {
					t.Fatalf("interfering delete: %v", err)
				}
			}
			return increment(data)
		})
		if err != nil {
			t.Fatal(err)
		}
		if document := mustGet(t, s, "counters", "c"); string(document.Data) != "1" {
			t.Fatalf("after Update: %q at v%d, want 1 (calls %d)", document.Data, document.Version, calls)
		}
	})
}

// TestUpdateRetriesUnboundedly checks Update keeps retrying however many times another writer intervenes.
func TestUpdateRetriesUnboundedly(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		for _, interferences := range []int{1, 2, 3, 4, 5, 8, 16, 49} {
			t.Run(fmt.Sprint(interferences), func(t *testing.T) {
				id := fmt.Sprintf("c%d", interferences)
				mustPut(t, s, "counters", id, []byte("0"), 0)
				calls := 0
				err := store.Update(context.Background(), s, "counters", id, func(data []byte) ([]byte, error) {
					calls++
					if calls <= interferences {
						if _, err := s.Put(context.Background(), "counters", id, []byte("0"), store.AnyVersion); err != nil {
							t.Fatalf("interfering write: %v", err)
						}
					}
					return increment(data)
				})
				if err != nil {
					t.Fatalf("Update gave up after %d calls with %d interfering writers: %v", calls, interferences, err)
				}
				if document := mustGet(t, s, "counters", id); string(document.Data) != "1" {
					t.Fatalf("after Update: %q", document.Data)
				}
			})
		}
	})
}

// TestUpdateFiftyGoroutinesCount checks concurrent increments through Update lose nothing.
func TestUpdateFiftyGoroutinesCount(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		var group sync.WaitGroup
		var failures atomic.Int32
		for g := 0; g < 50; g++ {
			group.Add(1)
			go func() {
				defer group.Done()
				if err := store.Update(context.Background(), s, "counters", "shared", increment); err != nil {
					failures.Add(1)
				}
			}()
		}
		group.Wait()
		document := mustGet(t, s, "counters", "shared")
		if string(document.Data) != "50" || document.Version != 50 {
			t.Fatalf("after 50 concurrent increments: %q at v%d (failures %d)", document.Data, document.Version, failures.Load())
		}
	})
}

// TestUpdateDoesNotAliasChangeResult checks mutating the returned slice after Update leaves the document unchanged.
func TestUpdateDoesNotAliasChangeResult(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		result := []byte("original")
		err := store.Update(context.Background(), s, "counters", "c", func([]byte) ([]byte, error) { return result, nil })
		if err != nil {
			t.Fatal(err)
		}
		copy(result, "MUTATED")
		if document := mustGet(t, s, "counters", "c"); string(document.Data) != "original" {
			t.Fatalf("Update aliased the change's result: %q", document.Data)
		}
	})
}

// TestUpdateReturnsSameDataStillWrites records whether an unchanged result still bumps the version.
func TestUpdateReturnsSameDataStillWrites(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		mustPut(t, s, "counters", "c", []byte("same"), 0)
		err := store.Update(context.Background(), s, "counters", "c", func(data []byte) ([]byte, error) { return data, nil })
		if err != nil {
			t.Fatal(err)
		}
		// PROBE: the contract does not say whether an identical result is written; either is defensible.
		document := mustGet(t, s, "counters", "c")
		t.Logf("version after an identical result: %d", document.Version)
		if !bytes.Equal(document.Data, []byte("same")) {
			t.Fatalf("document changed to %q", document.Data)
		}
	})
}

// TestUpdateRefusesInvalidNames checks Update validates names rather than looping or panicking.
func TestUpdateRefusesInvalidNames(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		calls := 0
		err := store.Update(context.Background(), s, "../escape", "x", func(data []byte) ([]byte, error) {
			calls++
			return []byte("x"), nil
		})
		if err == nil {
			t.Fatal("Update with a traversal collection succeeded")
		}
		if calls > 1 {
			t.Fatalf("change was called %d times for an invalid name", calls)
		}
	})
}

// TestUpdateHonoursCancelledContext records whether a cancelled context stops Update.
func TestUpdateHonoursCancelledContext(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// PROBE: the directory backend ignores ctx by signature; Update may or may not check it.
		err := store.Update(ctx, s, "counters", "c", func(data []byte) ([]byte, error) { return []byte("x"), nil })
		t.Logf("Update with a cancelled context: %v", err)
	})
}
