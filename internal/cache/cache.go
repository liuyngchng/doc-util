// Package cache provides a persistent, content-addressed result cache for the
// HTTP server. It is backed by a single bbolt file and enforces a TTL plus a
// total byte-size cap: entries older than the TTL are treated as misses, and
// when the cap is exceeded the oldest entries are evicted first.
package cache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Bucket names used by the server, one per endpoint so the same input file can
// be cached independently for each result type without key clashes.
const (
	BucketImage   = "image"
	BucketText    = "text"
	BucketDoc     = "doc"
	BucketExtract = "extract"
)

const (
	metaBucket    = "meta"
	totalBytesKey = "total_bytes"
)

// Options configures a Cache.
type Options struct {
	TTL      time.Duration // entries older than this are treated as misses
	MaxBytes int64         // evict oldest entries when total stored bytes exceed this
}

// Cache is a bbolt-backed result cache.
type Cache struct {
	db       *bolt.DB
	ttl      time.Duration
	maxBytes int64
}

// Open opens (creating if necessary) the cache at path.
func Open(path string, opts Options) (*Cache, error) {
	if opts.TTL <= 0 {
		opts.TTL = 7 * 24 * time.Hour
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 2 << 30 // 2 GiB
	}

	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open cache db: %w", err)
	}

	c := &Cache{db: db, ttl: opts.TTL, maxBytes: opts.MaxBytes}
	if err := c.init(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Cache) init() error {
	return c.db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{BucketImage, BucketText, BucketDoc, BucketExtract, metaBucket} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close closes the underlying database.
func (c *Cache) Close() error {
	return c.db.Close()
}

// Get returns the cached payload for (bucket, key). A missing or expired entry
// reports ok=false; expired entries are removed as a side effect.
func (c *Cache) Get(bucket string, key []byte) (payload []byte, ok bool) {
	_ = c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		v := b.Get(key)
		if v == nil {
			return nil
		}
		created, data, err := decode(v)
		if err != nil {
			_ = b.Delete(key)
			c.decTotal(tx, int64(len(v)))
			return nil
		}
		if time.Since(created) > c.ttl {
			_ = b.Delete(key)
			c.decTotal(tx, int64(len(v)))
			return nil
		}
		payload = append([]byte(nil), data...)
		ok = true
		return nil
	})
	return payload, ok
}

// Put stores payload under (bucket, key), evicting the oldest entries if the
// total stored size exceeds the configured cap.
func (c *Cache) Put(bucket string, key, data []byte) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		v := encode(time.Now(), data)
		if old := b.Get(key); old != nil {
			c.decTotal(tx, int64(len(old)))
		}
		if err := b.Put(key, v); err != nil {
			return err
		}
		c.incTotal(tx, int64(len(v)))
		return c.evict(tx)
	})
}

// evict removes the oldest entries (across all buckets) until the total stored
// size is back under the cap. A full scan is acceptable here because eviction
// only runs when a new entry pushes the cache over its size limit.
func (c *Cache) evict(tx *bolt.Tx) error {
	for c.total(tx) > c.maxBytes {
		var oldestBucket string
		var oldestKey []byte
		var oldestTime time.Time
		var oldestSize int64
		found := false

		for _, name := range []string{BucketImage, BucketText, BucketDoc, BucketExtract} {
			b := tx.Bucket([]byte(name))
			if b == nil {
				continue
			}
			cur := b.Cursor()
			for k, v := cur.First(); k != nil; k, v = cur.Next() {
				created, _, err := decode(v)
				if err != nil {
					continue
				}
				if !found || created.Before(oldestTime) {
					oldestBucket = name
					oldestKey = append(oldestKey[:0], k...)
					oldestTime = created
					oldestSize = int64(len(v))
					found = true
				}
			}
		}

		if !found {
			// No evictable entries (e.g. a single value larger than the cap).
			// Stop to avoid an infinite loop.
			return nil
		}
		if err := tx.Bucket([]byte(oldestBucket)).Delete(oldestKey); err != nil {
			return err
		}
		c.decTotal(tx, oldestSize)
	}
	return nil
}

// Clear removes every cached entry across all buckets and resets the stored
// total size to zero.
func (c *Cache) Clear() error {
	return c.db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{BucketImage, BucketText, BucketDoc, BucketExtract} {
			b := tx.Bucket([]byte(name))
			if b == nil {
				continue
			}
			cur := b.Cursor()
			for k, _ := cur.First(); k != nil; k, _ = cur.Next() {
				if err := cur.Delete(); err != nil {
					return err
				}
			}
		}
		c.setTotal(tx, 0)
		return nil
	})
}

// TotalBytes returns the current total stored size in bytes across all buckets.
func (c *Cache) TotalBytes() int64 {
	var n int64
	_ = c.db.View(func(tx *bolt.Tx) error {
		n = c.total(tx)
		return nil
	})
	return n
}

// encode wraps payload with an 8-byte big-endian Unix-seconds timestamp header.
func encode(created time.Time, data []byte) []byte {
	v := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(v[:8], uint64(created.Unix()))
	copy(v[8:], data)
	return v
}

// decode splits a stored value back into its creation time and payload.
func decode(v []byte) (time.Time, []byte, error) {
	if len(v) < 8 {
		return time.Time{}, nil, errors.New("cache value too short")
	}
	created := time.Unix(int64(binary.BigEndian.Uint64(v[:8])), 0)
	return created, v[8:], nil
}

// total reads the running total stored size from the meta bucket.
func (c *Cache) total(tx *bolt.Tx) int64 {
	v := tx.Bucket([]byte(metaBucket)).Get([]byte(totalBytesKey))
	if len(v) < 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(v))
}

func (c *Cache) setTotal(tx *bolt.Tx, n int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	_ = tx.Bucket([]byte(metaBucket)).Put([]byte(totalBytesKey), b[:])
}

func (c *Cache) incTotal(tx *bolt.Tx, delta int64) { c.setTotal(tx, c.total(tx)+delta) }
func (c *Cache) decTotal(tx *bolt.Tx, delta int64) { c.setTotal(tx, c.total(tx)-delta) }
