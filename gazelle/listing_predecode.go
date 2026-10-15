package typescript

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

var parallelMode = "on"

var errPredecodeMismatch = errors.New("a listing decoded ahead of the walk differs from its decode on the walk")

// The walk reaches listings one at a time on the main goroutine; decoding them ahead takes that off its path.
var predecodes listingPredecode

const maxPredecodeWorkers = 8

// takenListingsFile names the keys the last full run asked for, so a cache
// holding entries no run reads any more decodes only the live ones.
const takenListingsFile = "listings-taken"

type listingPredecode struct {
	mu            sync.Mutex
	dir           string
	jobs          map[string]*predecodeJob
	taken         map[string]bool
	verify        bool
	stopped       atomic.Bool
	ahead, onWalk atomic.Int64
	wg            sync.WaitGroup
}

type predecodeJob struct {
	key     string
	once    sync.Once
	ahead   bool
	decoded decodedListing
}

type decodedListing struct {
	entry *cachedListing
	fresh bool
	sum   [sha256.Size]byte
	err   error
}

func (d decodedListing) result() (*cachedListing, bool, error) {
	return d.entry, d.fresh, d.err
}

// matches reports whether two decodes of one key read the same bytes to the same outcome.
func (d decodedListing) matches(again decodedListing) bool {
	return (d.err == nil) == (again.err == nil) && d.sum == again.sum && d.fresh == again.fresh
}

func (j *predecodeJob) run(ahead bool) {
	j.once.Do(func() {
		j.ahead = ahead
		j.decoded = decodeListing(j.key)
	})
}

// decodeListing reads key's entry and whether everything it read is unchanged.
func decodeListing(key string) decodedListing {
	file := filepath.Join(listingCacheDir, key)
	data, err := readListingFile(file)
	if err != nil {
		return decodedListing{err: err}
	}
	entry, err := decodeEntry(file, data)
	if err != nil {
		return decodedListing{err: err}
	}
	return decodedListing{
		entry: entry,
		fresh: listingFingerprint(entry.Reads, entry.Dirs) == entry.Fingerprint,
		sum:   sha256.Sum256(data),
	}
}

func isListingKey(name string) bool {
	if len(name) != 2*sha256.Size {
		return false
	}
	for _, c := range []byte(name) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// predecodeKeys are the keys the last full run took, or every entry when no run recorded them.
func predecodeKeys(dir string) []string {
	var keys []string
	if data, err := os.ReadFile(filepath.Join(dir, takenListingsFile)); err == nil {
		for key := range strings.Lines(string(data)) {
			if key = strings.TrimSuffix(key, "\n"); isListingKey(key) {
				keys = append(keys, key)
			}
		}
		return keys
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && isListingKey(entry.Name()) {
			keys = append(keys, entry.Name())
		}
	}
	return keys
}

func (d *listingPredecode) start(dir string, verify bool) {
	keys := predecodeKeys(dir)
	queue := make([]*predecodeJob, len(keys))
	jobs := make(map[string]*predecodeJob, len(keys))
	for i, key := range keys {
		queue[i] = &predecodeJob{key: key}
		jobs[key] = queue[i]
	}
	d.mu.Lock()
	d.dir, d.jobs, d.taken, d.verify = dir, jobs, map[string]bool{}, verify
	d.mu.Unlock()
	d.stopped.Store(false)
	d.ahead.Store(0)
	d.onWalk.Store(0)
	var next atomic.Int64
	for range min(len(queue), maxPredecodeWorkers, runtime.GOMAXPROCS(0)) {
		d.wg.Go(func() {
			for !d.stopped.Load() {
				i := int(next.Add(1)) - 1
				if i >= len(queue) {
					return
				}
				queue[i].run(true)
			}
		})
	}
}

// take is key's entry, decoded ahead when a worker reached it first, and now otherwise.
func (d *listingPredecode) take(key string) (entry *cachedListing, fresh bool, err error) {
	d.mu.Lock()
	job, verify := d.jobs[key], d.verify
	delete(d.jobs, key)
	if d.taken != nil {
		d.taken[key] = true
	}
	d.mu.Unlock()
	if job == nil {
		return decodeListing(key).result()
	}
	job.run(false)
	if !job.ahead {
		d.onWalk.Add(1)
		return job.decoded.result()
	}
	d.ahead.Add(1)
	if verify && !job.decoded.matches(decodeListing(key)) {
		return nil, false, fmt.Errorf("typescript: -ts_parallel=verify: %w: %s", errPredecodeMismatch, key)
	}
	return job.decoded.result()
}

// finish stops the workers, waits for them, drops every entry the run never
// took, and records the keys it took for the next run.
func (d *listingPredecode) finish() {
	d.stopped.Store(true)
	d.wg.Wait()
	d.mu.Lock()
	dir, taken := d.dir, d.taken
	d.dir, d.jobs, d.taken = "", nil, nil
	d.mu.Unlock()
	if dir == "" {
		return
	}
	if listingVerbose {
		log.Printf("typescript: listing cache predecode: %d decoded ahead, %d on the walk", d.ahead.Load(), d.onWalk.Load())
	}
	var list strings.Builder
	for _, key := range slices.Sorted(maps.Keys(taken)) {
		list.WriteString(key)
		list.WriteByte('\n')
	}
	writeTakenListings(dir, list.String())
}

// An unchanged list is not rewritten, so an unchanged tree leaves the cache directory as it was.
func writeTakenListings(dir, list string) {
	if list == "" {
		return
	}
	if old, err := os.ReadFile(filepath.Join(dir, takenListingsFile)); err == nil && string(old) == list {
		return
	}
	writeAtomically(filepath.Join(dir, takenListingsFile), []byte(list))
}
