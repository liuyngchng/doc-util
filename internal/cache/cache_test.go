package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T, opts Options) *Cache {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	c, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestPutGet(t *testing.T) {
	c := openTemp(t, Options{TTL: time.Hour, MaxBytes: 1 << 20})

	if _, ok := c.Get(BucketImage, []byte("k1")); ok {
		t.Fatal("expected miss before Put")
	}
	if err := c.Put(BucketImage, []byte("k1"), []byte("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := c.Get(BucketImage, []byte("k1"))
	if !ok {
		t.Fatal("expected hit after Put")
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestBucketsAreIndependent(t *testing.T) {
	c := openTemp(t, Options{TTL: time.Hour, MaxBytes: 1 << 20})

	_ = c.Put(BucketImage, []byte("same"), []byte("image"))
	_ = c.Put(BucketText, []byte("same"), []byte("text"))

	if got, ok := c.Get(BucketImage, []byte("same")); !ok || string(got) != "image" {
		t.Fatalf("image bucket: got %q ok=%v", got, ok)
	}
	if got, ok := c.Get(BucketText, []byte("same")); !ok || string(got) != "text" {
		t.Fatalf("text bucket: got %q ok=%v", got, ok)
	}
}

func TestExpiry(t *testing.T) {
	c := openTemp(t, Options{TTL: 10 * time.Millisecond, MaxBytes: 1 << 20})

	_ = c.Put(BucketDoc, []byte("k"), []byte("v"))
	time.Sleep(20 * time.Millisecond)

	if _, ok := c.Get(BucketDoc, []byte("k")); ok {
		t.Fatal("expected expired entry to miss")
	}
}

func TestEviction(t *testing.T) {
	// Each entry is 8 (timestamp) + 11 (payload) = 19 bytes; two entries are
	// 38 bytes. Cap at 30 so the second Put forces eviction of the first.
	c := openTemp(t, Options{TTL: time.Hour, MaxBytes: 30})

	_ = c.Put(BucketImage, []byte("old"), []byte("payload-old"))
	time.Sleep(2 * time.Millisecond)
	_ = c.Put(BucketText, []byte("new"), []byte("payload-new"))

	// "old" (older) should have been evicted; "new" should remain.
	if _, ok := c.Get(BucketImage, []byte("old")); ok {
		t.Fatal("expected oldest entry to be evicted")
	}
	if got, ok := c.Get(BucketText, []byte("new")); !ok || string(got) != "payload-new" {
		t.Fatalf("newest entry: got %q ok=%v", got, ok)
	}
}

func TestCloseRemovesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	c, err := Open(path, Options{TTL: time.Hour, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = c.Put(BucketExtract, []byte("k"), []byte("v"))
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file should persist after Close: %v", err)
	}
}

func TestClear(t *testing.T) {
	c := openTemp(t, Options{TTL: time.Hour, MaxBytes: 1 << 20})

	// Put entries in every bucket.
	_ = c.Put(BucketImage, []byte("img1"), []byte("a"))
	_ = c.Put(BucketText, []byte("txt1"), []byte("b"))
	_ = c.Put(BucketDoc, []byte("doc1"), []byte("c"))
	_ = c.Put(BucketExtract, []byte("ext1"), []byte("d"))

	if err := c.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	for _, b := range []string{BucketImage, BucketText, BucketDoc, BucketExtract} {
		if _, ok := c.Get(b, []byte("img1")); ok {
			t.Fatalf("expected miss in bucket %s after Clear", b)
		}
		if _, ok := c.Get(b, []byte("txt1")); ok {
			t.Fatalf("expected miss in bucket %s after Clear", b)
		}
		if _, ok := c.Get(b, []byte("doc1")); ok {
			t.Fatalf("expected miss in bucket %s after Clear", b)
		}
		if _, ok := c.Get(b, []byte("ext1")); ok {
			t.Fatalf("expected miss in bucket %s after Clear", b)
		}
	}

	// Re-populate after Clear: cache should work normally.
	if err := c.Put(BucketImage, []byte("newk"), []byte("newv")); err != nil {
		t.Fatalf("Put after Clear: %v", err)
	}
	if got, ok := c.Get(BucketImage, []byte("newk")); !ok || string(got) != "newv" {
		t.Fatalf("after Clear: got %q ok=%v, want %q", got, ok, "newv")
	}
}
