// Package retention deletes recordings older than a configured age: it removes
// the segment files from disk, their rows from the index, and any directories
// left empty.
package retention

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"eneverre/internal/media/index"
	"eneverre/internal/media/recstore"
)

// Tuning knobs for the purge loop. The defaults are sized for a busy
// NVR with thousands of segments per day: small enough that a single
// batch doesn't hold a long-running SQLite write transaction (which
// would block the recorder's per-segment inserts under WAL), large
// enough that the wall-clock cost per batch is dominated by parallel
// file I/O, not by SQL or fsync overhead.
const (
	purgeBatchSize   = 500  // segments per query + delete batch
	purgeFileWorkers = 4    // parallel os.Remove goroutines per batch
	purgeBatchPause  = 50 * time.Millisecond // yield between batches
)

// Cleaner periodically removes expired recordings.
type Cleaner struct {
	Index      *index.Index
	Retain     time.Duration // max age; must be > 0
	RecordPath string        // record-path pattern, used to find empty dirs to prune
	Logf       func(string, ...any)

	interval time.Duration
}

// Run loops until ctx is cancelled, cleaning at a computed interval.
func (c *Cleaner) Run(ctx context.Context) {
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	// clean often enough to bound overshoot, but not too often
	c.interval = c.Retain / 10
	if c.interval < time.Minute {
		c.interval = time.Minute
	}
	if c.interval > time.Hour {
		c.interval = time.Hour
	}

	c.clean()
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.clean()
		case <-ctx.Done():
			return
		}
	}
}

// clean drains every expired segment in batches: query N, delete N
// files in parallel, delete N rows in a single transaction, yield to
// the recorder, repeat. Bounding each batch keeps the SQLite write
// transaction short (one fsync per batch instead of one per segment)
// and the read transaction short (the query result set is bounded).
func (c *Cleaner) clean() {
	cutoff := time.Now().Add(-c.Retain)

	totalRemoved := 0
	for {
		expired, err := c.Index.Expired(cutoff, purgeBatchSize)
		if err != nil {
			c.Logf("query expired: %v", err)
			return
		}
		if len(expired) == 0 {
			break
		}

		fpaths := make([]string, len(expired))
		for i, s := range expired {
			fpaths[i] = s.Fpath
		}
		// Parallel os.Remove: file I/O is the bottleneck (the index
		// delete is one transaction, one fsync). Limited concurrency
		// avoids thrashing SSDs and avoids spamming the kernel with
		// concurrent syscalls.
		deleted := deleteFilesParallel(fpaths, purgeFileWorkers, c.Logf)
		// One transaction, one fsync, for all successfully-deleted
		// rows. Rows whose file failed to delete (e.g. permission)
		// stay in the index and are retried next pass.
		if err := c.Index.DeleteBatch(deleted); err != nil {
			c.Logf("index delete batch: %v", err)
			return
		}

		totalRemoved += len(deleted)
		c.Logf("purged %d expired segment(s) (older than %s)", len(deleted), c.Retain)

		// Targeted dir prune: only walk the parents of just-deleted
		// files (the previous implementation walked the whole record
		// tree on every clean, which becomes O(tree size) per pass).
		c.pruneEmptyDirsAround(deleted)

		// Yield so the recorder's per-segment INSERTs can interleave
		// with the next batch. Without this, a continuous purge
		// (thousands of segments) could starve the writer of WAL
		// checkpoints.
		time.Sleep(purgeBatchPause)
	}

	if totalRemoved > 0 {
		c.Logf("purge pass complete: %d segment(s) removed (older than %s)", totalRemoved, c.Retain)
	}
}

// deleteFilesParallel removes the given file paths with up to `workers`
// goroutines, returning the subset that was successfully removed (or
// did not exist). Failed removes are logged and left in the index so
// the next retention pass retries them.
func deleteFilesParallel(fpaths []string, workers int, logf func(string, ...any)) []string {
	if workers < 1 {
		workers = 1
	}
	if len(fpaths) == 0 {
		return nil
	}
	jobs := make(chan string, len(fpaths))
	for _, fp := range fpaths {
		jobs <- fp
	}
	close(jobs)

	var (
		mu  sync.Mutex
		out = make([]string, 0, len(fpaths))
		wg  sync.WaitGroup
	)
	worker := func() {
		defer wg.Done()
		for fp := range jobs {
			if err := os.Remove(fp); err == nil || os.IsNotExist(err) {
				mu.Lock()
				out = append(out, fp)
				mu.Unlock()
			} else if logf != nil {
				logf("remove %s: %v (will retry next pass)", fp, err)
			}
		}
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	return out
}

// pruneEmptyDirsAround walks up from each just-deleted file's parent
// directory and rmdir's the empty ones, stopping at the recording root
// or at a non-empty directory. Much cheaper than the previous full-tree
// walk when only a few files were deleted: O(deleted files × depth)
// instead of O(record tree size).
func (c *Cleaner) pruneEmptyDirsAround(fpaths []string) {
	root := recstore.CommonPath(c.RecordPath)
	if root == "" {
		return
	}
	seen := make(map[string]struct{}, len(fpaths)*2)
	for _, fp := range fpaths {
		d := filepath.Dir(fp)
		for {
			if d == root || d == "/" || d == "." || d == "" {
				break
			}
			if _, ok := seen[d]; ok {
				break
			}
			seen[d] = struct{}{}
			if err := os.Remove(d); err != nil {
				// not empty (most common) or not writable — stop climbing
				break
			}
			d = filepath.Dir(d)
		}
	}
}
