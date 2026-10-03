package tlatrace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Case struct {
	Module     string   `json:"module"`
	Config     string   `json:"config"`
	Initial    int64    `json:"initial,omitempty"`
	Distinct   int64    `json:"distinct,omitempty"`
	Generated  int64    `json:"generated,omitempty"`
	Violations []string `json:"violations,omitempty"`
	Replay     bool     `json:"replay,omitempty"`
}

type Runner struct {
	Java, Jar, Models, Output string
	Workers                   int
	Cleanup                   time.Duration
}

func VerifyArtifact(path, digest string) error {
	want, err := hex.DecodeString(digest)
	if err != nil || len(want) != sha256.Size {
		return errors.New("TLC requires a SHA256 artifact identity")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if !slices.Equal(hash.Sum(nil), want) {
		return fmt.Errorf("TLC artifact SHA256 mismatch for %q", path)
	}
	return nil
}

func (r Runner) Run(ctx context.Context, c Case) ([]State, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("TLC requires the caller's aggregate deadline")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(r.Java) || !filepath.IsAbs(r.Jar) || !filepath.IsAbs(r.Models) || !filepath.IsAbs(r.Output) || r.Workers < 1 || r.Cleanup <= 0 {
		return nil, errors.New("TLC requires absolute input paths, workers and a cleanup allowance")
	}
	if filepath.Base(c.Config) != c.Config || !strings.HasSuffix(c.Config, ".cfg") || !identifier.MatchString(c.Module) {
		return nil, errors.New("TLC requires a module identifier and a config filename")
	}
	if len(c.Violations) == 0 && (c.Initial <= 0 || c.Distinct <= 0 || c.Generated <= 0 || c.Replay) {
		return nil, errors.New("positive TLC checks require an exact census")
	}
	directory := filepath.Join(r.Output, strings.TrimSuffix(c.Config, ".cfg"))
	if err := os.Mkdir(directory, 0o755); err != nil {
		return nil, err
	}
	log, err := os.Create(filepath.Join(directory, "tlc.log"))
	if err != nil {
		return nil, err
	}
	tracePath := filepath.Join(directory, "trace.json")
	workers := r.Workers
	if c.Replay {
		workers = 1
	}
	args := []string{"-XX:ActiveProcessorCount=" + strconv.Itoa(r.Workers), "-Djava.io.tmpdir=" + directory,
		"-cp", r.Jar, "tlc2.TLC", "-workers", strconv.Itoa(workers), "-noGenerateSpecTE",
		"-config", c.Config, "-metadir", filepath.Join(directory, "states")}
	if len(c.Violations) != 0 {
		args = append(args, "-dumpTrace", "json", tracePath)
	}
	args = append(args, c.Module)
	command := exec.CommandContext(ctx, r.Java, args...)
	command.Dir = r.Models
	command.Stdout, command.Stderr = log, log
	command.WaitDelay = r.Cleanup
	runErr := command.Run()
	closeErr := log.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	code := 0
	if runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) {
			return nil, runErr
		}
		code = exit.ExitCode()
	}
	output, err := os.ReadFile(log.Name())
	if err != nil {
		return nil, err
	}
	if err := accept(c, code, string(output)); err != nil {
		return nil, fmt.Errorf("%s: %w (log %s)", c.Config, err, log.Name())
	}
	if len(c.Violations) == 0 {
		return nil, nil
	}
	trace, err := os.ReadFile(tracePath)
	if err != nil {
		return nil, err
	}
	return Decode(trace)
}

var (
	identifier   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	initials     = regexp.MustCompile(`(?m)^Finished computing initial states: ([0-9,]+) distinct states? generated at `)
	census       = regexp.MustCompile(`(?m)^([0-9,]+) states generated, ([0-9,]+) distinct states found, ([0-9,]+) states left on queue\.`)
	violation    = regexp.MustCompile(`^Invariant ([A-Za-z][A-Za-z0-9_]*) is violated(?:\.| by the initial state:)$`)
	nativeErrors = regexp.MustCompile(`(?m)^Error: ?(.*)$`)
)

func accept(c Case, code int, output string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w\n%s", err, output)
		}
	}()
	reports := nativeErrors.FindAllStringSubmatch(output, -1)
	if len(c.Violations) != 0 {
		if code != 12 || len(reports) == 0 {
			return fmt.Errorf("expected native exit 12 and one named invariant in %v, got exit %d", c.Violations, code)
		}
		matched := violation.FindStringSubmatch(reports[0][1])
		if matched == nil || !slices.Contains(c.Violations, matched[1]) {
			return fmt.Errorf("unexpected invariant failure %q", reports[0][1])
		}
		initial := strings.HasSuffix(reports[0][1], "by the initial state:")
		if (initial && len(reports) != 1) || (!initial && (len(reports) != 2 || reports[1][1] != "The behavior up to this point is:")) {
			return fmt.Errorf("TLC reported additional or incomplete errors: %v", reports)
		}
		return nil
	}
	if code != 0 || !strings.Contains(output, "Model checking completed. No error has been found.") || len(reports) != 0 {
		return fmt.Errorf("positive model did not exhaust successfully, native exit %d", code)
	}
	i, counts := initials.FindAllStringSubmatch(output, -1), census.FindAllStringSubmatch(output, -1)
	if len(i) != 1 || len(counts) != 1 {
		return errors.New("positive model omitted its unique initial or final state census")
	}
	for _, value := range []struct {
		text string
		want int64
	}{{i[0][1], c.Initial}, {counts[0][1], c.Generated}, {counts[0][2], c.Distinct}, {counts[0][3], 0}} {
		got, err := strconv.ParseInt(strings.ReplaceAll(value.text, ",", ""), 10, 64)
		if err != nil || got != value.want {
			return fmt.Errorf("state census %q, expected %d", value.text, value.want)
		}
	}
	return nil
}
