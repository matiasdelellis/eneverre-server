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
	purgeBatchSize   = 500                   // segments per query + delete batch
	purgeFileWorkers = 4                     // parallel os.Remove goroutines per batch
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
		deleted, err := c.purgeBatch(expired)
		if err != nil {
			c.Logf("index delete batch: %v", err)
			return
		}
		totalRemoved += deleted
		c.Logf("purged %d expired segment(s) (older than %s)", deleted, c.Retain)
	}

	if totalRemoved > 0 {
		c.Logf("purge pass complete: %d segment(s) removed (older than %s)", totalRemoved, c.Retain)
	}
}

// PurgeToFree deletes the oldest segments — ignoring the retain window — until
// statfs(dir) reports at least `target` free bytes, the index runs empty, or
// ctx is cancelled. It returns the number of segments removed.
//
// This is the emergency valve behind the media engine's low-disk watcher:
// where clean() drops footage that has aged out, PurgeToFree sacrifices the
// oldest footage regardless of age to keep the recording volume from hitting
// ENOSPC. It shares clean()'s batch machinery (parallel unlink, one index
// transaction per batch, dir prune, inter-batch yield); the only differences
// are the query (oldest-first instead of expired) and the stop condition
// (free space instead of an empty result). statfs is injected so tests can
// drive the loop without a real disk; production passes diskfree.Available.
//
// The free-space check runs once per batch (at the top of each iteration),
// not per segment, so the purge can overshoot the target by at most one
// batch (purgeBatchSize segments) before it notices it has crossed. That
// slack is acceptable: `target` is already the high-water mark (2x the
// low-water), so overshooting leaves extra headroom rather than parking the
// disk right at the edge, and the disk monitor won't re-fire OnLow until free
// climbs back above 2*LowWater — no cascading re-purges. Shrink purgeBatchSize
// if a tighter bound is ever needed.
func (c *Cleaner) PurgeToFree(ctx context.Context, dir string, target uint64, statfs func(string) (uint64, error)) int {
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	totalRemoved := 0
	for {
		if ctx.Err() != nil {
			return totalRemoved
		}
		// Re-check before each batch so a purge that already recovered the
		// disk stops instead of deleting more than necessary.
		free, err := statfs(dir)
		if err != nil {
			c.Logf("emergency purge: statfs %s: %v", dir, err)
			return totalRemoved
		}
		if free >= target {
			return totalRemoved
		}
		batch, err := c.Index.Oldest(purgeBatchSize)
		if err != nil {
			c.Logf("emergency purge: query oldest: %v", err)
			return totalRemoved
		}
		if len(batch) == 0 {
			// No footage left to sacrifice and still below target: the
			// volume is genuinely full of non-recording data. Loud so the
			// operator knows to free space by hand.
			c.Logf("emergency purge: index empty but only %d byte(s) free (target %d) — free space manually", free, target)
			return totalRemoved
		}
		deleted, err := c.purgeBatch(batch)
		if err != nil {
			c.Logf("emergency purge: index delete batch: %v", err)
			return totalRemoved
		}
		totalRemoved += deleted
		c.Logf("emergency purge: removed %d oldest segment(s) (%d byte(s) free, target %d)", deleted, free, target)
	}
}

// purgeBatch deletes one batch of segment files in parallel, drops the
// successfully-removed rows in a single index transaction, and prunes any
// directories left empty. It returns the number of segments removed. Shared
// by the age-based sweep (clean) and the free-space emergency purge
// (PurgeToFree).
func (c *Cleaner) purgeBatch(segs []index.Segment) (int, error) {
	fpaths := make([]string, len(segs))
	for i, s := range segs {
		fpaths[i] = s.Fpath
	}
	// Parallel os.Remove: file I/O is the bottleneck (the index delete is
	// one transaction, one fsync). Limited concurrency avoids thrashing
	// SSDs and avoids spamming the kernel with concurrent syscalls.
	deleted := deleteFilesParallel(fpaths, purgeFileWorkers, c.Logf)
	// One transaction, one fsync, for all successfully-deleted rows. Rows
	// whose file failed to delete (e.g. permission) stay in the index and
	// are retried next pass.
	if err := c.Index.DeleteBatch(deleted); err != nil {
		return 0, err
	}
	// Targeted dir prune: only walk the parents of just-deleted files (a
	// full-tree walk would be O(tree size) per batch).
	c.pruneEmptyDirsAround(deleted)
	// Yield so the recorder's per-segment INSERTs can interleave with the
	// next batch. Without this a continuous purge (thousands of segments)
	// could starve the writer of WAL checkpoints.
	time.Sleep(purgeBatchPause)
	return len(deleted), nil
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
