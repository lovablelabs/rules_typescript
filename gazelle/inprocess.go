package typescript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/microsoft/typescript-go/shim/listing"
)

// listingInProcessMode is -ts_listing_inproc: on, verify or off.
var listingInProcessMode = "on"

// startTsgo starts a listing's tsgo process; a test counts them through it.
var startTsgo = (*exec.Cmd).Start

// One gate per tsgo binary for the run, so every listing shares its parses.
var inProcessGates sync.Map

var errInProcessOff = errors.New("-ts_listing_inproc=off")

// inProcessCompiler lists as tsgo would when tsgo is a release of the
// TypeScript version linked in; otherwise it is nil, and err says why.
func inProcessCompiler(tsgo string) (*listing.Compiler, error) {
	if listingInProcessMode == "off" {
		return nil, errInProcessOff
	}
	gate, _ := inProcessGates.LoadOrStore(tsgo, sync.OnceValues(func() (*listing.Compiler, error) {
		return releaseCompiler(tsgo)
	}))
	return gate.(func() (*listing.Compiler, error))()
}

// listingHow is how the listings of tsgo run, for -ts_verbose.
func listingHow(tsgo string) string {
	if _, err := inProcessCompiler(tsgo); err != nil {
		return "through tsgo, not in process: " + err.Error()
	}
	return "in process through typescript-go " + listing.Version()
}

// releaseCompiler reads tsgo's package.json from beside it or its lib
// directory, where a release tsgo reads its lib files from.
func releaseCompiler(tsgo string) (*listing.Compiler, error) {
	resolved, err := filepath.EvalSymlinks(tsgo)
	if err != nil {
		return nil, err
	}
	libDir := filepath.Dir(resolved)
	if _, err := os.Stat(filepath.Join(libDir, "lib.d.ts")); err != nil {
		return nil, fmt.Errorf("no lib.d.ts beside %s, as a release has: %w", resolved, err)
	}
	var manifest struct{ Name, Version string }
	found := ""
	for _, candidate := range []string{filepath.Join(filepath.Dir(tsgo), "package.json"), filepath.Join(filepath.Dir(libDir), "package.json")} {
		data, err := os.ReadFile(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("%s: %w", candidate, err)
		}
		if manifest.Name != "typescript" && !strings.HasPrefix(manifest.Name, "@typescript/") {
			return nil, fmt.Errorf("%s names %q, not a TypeScript package", candidate, manifest.Name)
		}
		found = candidate
		break
	}
	switch {
	case found == "":
		return nil, fmt.Errorf("no package.json beside %s or its lib directory", tsgo)
	case manifest.Version == "":
		return nil, fmt.Errorf("%s names no version", found)
	case manifest.Version != listing.Version():
		return nil, fmt.Errorf("%s is %s %s and the typescript-go linked in is %s", found, manifest.Name, manifest.Version, listing.Version())
	}
	return listing.New(libDir), nil
}

// listInProcess is runListingUncached through compiler: tsgo's output fed
// to the same decoding, its exit status as the error tsgo's would be.
func listInProcess(ctx context.Context, compiler *listing.Compiler, repoRoot, subject string, args []string) (*program, error) {
	var facts resolutionFacts
	split := newTraceSplitter(repoRoot, &facts)
	// A line the decode refuses stops the listing, as the pipe's reader kills tsgo.
	listingCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	out := newLineWriter(split.line, stop)
	status, err := compiler.Run(listingCtx, repoRoot, args, out)
	if decodeErr := out.close(); decodeErr != nil {
		err = decodeErr
	} else if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: %w", subject, context.Cause(ctx))
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", subject, err)
	}
	var runErr error
	if status != 0 {
		runErr = exitStatus(status)
	}
	return programOfOutput(split, subject, args, runErr, "")
}

// exitStatus is a status tsgo would exit with, spelt as *exec.ExitError spells it.
type exitStatus int

func (s exitStatus) Error() string {
	return fmt.Sprintf("exit status %d", int(s))
}

// verifyInProcess is -ts_listing_inproc=verify: tsgo must list what got holds.
func verifyInProcess(ctx context.Context, repoRoot, tsgo, subject string, args []string, got *program) {
	want, err := listThroughTsgo(ctx, repoRoot, tsgo, subject, args)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Fatalf("typescript: -ts_listing_inproc=verify: %v", err)
	}
	if diff := inProcessDifference(got, want); diff != "" {
		log.Fatalf("typescript: -ts_listing_inproc=verify: the in-process listing of %s differs from tsgo's:\n%s", subject, diff)
	}
}

// inProcessDifference is how got, listed in process, differs from want,
// listed by tsgo, as listingDifference has it and then field by field.
func inProcessDifference(got, want *program) string {
	if diff := listingDifference(got, want); diff != "" {
		return diff
	}
	var diff []string
	for _, field := range []struct {
		name      string
		got, want any
	}{
		{"listing", got.Listing, want.Listing},
		{"resolutions", got.candidates, want.candidates},
		{"resolution facts", got.resolution, want.resolution},
		{"refusal", got.refused, want.refused},
	} {
		if !reflect.DeepEqual(field.got, field.want) {
			diff = append(diff, field.name+" differs")
		}
	}
	return strings.Join(diff, "\n")
}

// lineWriter hands line each line written to it, newline kept, on a
// goroutine of its own and a chunk of lines at a time, as eachLine does a
// pipe's; the first error stops it and is handed to stop. One writer at a
// time: tsgo writes its trace and its listing from one goroutine. A chunk's
// buffer is reused once its lines are handled, so line must keep none of them.
type lineWriter struct {
	buf    *[]byte
	chunks chan *[]byte
	done   sync.WaitGroup
	err    error
}

const lineChunk = 1 << 20

var lineBuffers = sync.Pool{New: func() any { return new([]byte) }}

func newLineWriter(line func(string) error, stop func(error)) *lineWriter {
	w := &lineWriter{chunks: make(chan *[]byte, 4), buf: lineBuffers.Get().(*[]byte)}
	*w.buf = (*w.buf)[:0]
	w.done.Go(func() {
		for b := range w.chunks {
			chunk := unsafe.String(unsafe.SliceData(*b), len(*b))
			for chunk != "" && w.err == nil {
				n := strings.IndexByte(chunk, '\n') + 1
				if n == 0 {
					n = len(chunk)
				}
				if w.err = line(chunk[:n]); w.err != nil {
					stop(w.err)
				}
				chunk = chunk[n:]
			}
			lineBuffers.Put(b)
		}
	})
	return w
}

func (w *lineWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	if len(*w.buf) >= lineChunk {
		w.send(bytes.LastIndexByte(*w.buf, '\n') + 1)
	}
	return len(p), nil
}

func (w *lineWriter) send(end int) {
	if end > 0 {
		next := lineBuffers.Get().(*[]byte)
		*next = append((*next)[:0], (*w.buf)[end:]...)
		*w.buf = (*w.buf)[:end]
		w.chunks <- w.buf
		w.buf = next
	}
}

// close hands on what is left, an unterminated last line included, and waits
// for every line to be handed on.
func (w *lineWriter) close() error {
	w.send(len(*w.buf))
	close(w.chunks)
	w.done.Wait()
	lineBuffers.Put(w.buf)
	w.buf = nil
	return w.err
}
