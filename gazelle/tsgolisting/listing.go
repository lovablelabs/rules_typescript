// Package listing runs a tsgo listing in the calling process: the command
// line, the output and the exit status tsgo's own main would have, sharing
// parses between the listings a Compiler runs.
package listing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/diagnostics"
	"github.com/microsoft/typescript-go/internal/execute/tsc"
	"github.com/microsoft/typescript-go/internal/locale"
	"github.com/microsoft/typescript-go/internal/parser"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/tspath"
	"github.com/microsoft/typescript-go/internal/vfs"
	"github.com/microsoft/typescript-go/internal/vfs/osvfs"
)

// ErrUnsupported is a command line Run leaves to tsgo: one that is not a
// --listFilesOnly listing of a project or of files, or one whose output
// depends on more than its inputs (timings, a build info file, a watch).
var ErrUnsupported = errors.New("not a listing this package runs")

// Version is the TypeScript version of the compiler linked in.
func Version() string {
	return core.Version()
}

// Compiler runs listings against one directory of lib files and, until
// DropParses, keeps every file it parses for the next listing that reads it.
type Compiler struct {
	libDir    string
	files     collections.SyncMap[fileKey, *parsedFile]
	templates collections.SyncMap[templateKey, string]
}

type fileKey struct {
	options ast.SourceFileParseOptions
	kind    core.ScriptKind
}

type parsedFile struct {
	once sync.Once
	text string
	file *ast.SourceFile
}

// New lists with the lib files in libDir, the directory a release tsgo reads
// them from: its executable's, symlinks resolved.
func New(libDir string) *Compiler {
	return &Compiler{libDir: tspath.NormalizePath(libDir)}
}

// DropParses frees every file c keeps; a later listing parses them again.
func (c *Compiler) DropParses() {
	c.files.Clear()
}

// Run is `tsgo args` run in dir with stdout as its standard output, and
// returns the status tsgo would exit with. Cancelling ctx makes the
// listing's output incomplete, so Run then returns ctx's error. A panic on
// the calling goroutine is returned as an error; one on a goroutine tsgo
// parses on ends the process, as it ends tsgo's.
func (c *Compiler) Run(ctx context.Context, dir string, args []string, stdout io.Writer) (status int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(args) > 0 && slices.Contains([]string{"-b", "--b", "-build", "--build"}, strings.ToLower(args[0])) {
		return 0, ErrUnsupported
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("typescript-go %s panicked: %v\n%s", Version(), r, debug.Stack())
		}
	}()
	sys := &system{
		stdout: stdout,
		fs:     osvfs.FS(),
		libDir: c.libDir,
		cwd:    tspath.NormalizePath(dir),
		start:  time.Now(),
	}
	status, err = c.compile(ctx, sys, tsoptions.ParseCommandLine(args, sys))
	if err == nil {
		err = ctx.Err()
	}
	return status, err
}

// compile is tsgo's tscCompilation for a command line that neither builds,
// watches, nor writes anything but its listing.
func (c *Compiler) compile(ctx context.Context, sys *system, commandLine *tsoptions.ParsedCommandLine) (int, error) {
	locale := commandLine.Locale()
	options := commandLine.CompilerOptions()
	reportDiagnostic := tsc.CreateDiagnosticReporter(sys, sys.Writer(), locale, options)
	if len(commandLine.Errors) > 0 {
		for _, e := range commandLine.Errors {
			reportDiagnostic(e)
		}
		return int(tsc.ExitStatusDiagnosticsPresent_OutputsSkipped), nil
	}
	if !options.ListFilesOnly.IsTrue() || options.PprofDir != "" || options.Init.IsTrue() ||
		options.Version.IsTrue() || options.Help.IsTrue() || options.All.IsTrue() ||
		options.Watch.IsTrue() || options.ShowConfig.IsTrue() {
		return 0, ErrUnsupported
	}
	configFileName := ""
	if options.Project != "" {
		if len(commandLine.FileNames()) != 0 {
			return 0, ErrUnsupported
		}
		project := tspath.NormalizePath(options.Project)
		if sys.FS().DirectoryExists(project) {
			project = tspath.CombinePaths(project, "tsconfig.json")
		}
		if !sys.FS().FileExists(project) {
			return 0, ErrUnsupported
		}
		configFileName = project
	} else if !options.IgnoreConfig.IsTrue() || len(commandLine.FileNames()) == 0 {
		return 0, ErrUnsupported
	}

	config := commandLine
	extendedConfigCache := &tsc.ExtendedConfigCache{}
	if configFileName != "" {
		var commandLineRaw *collections.OrderedMap[string, any]
		if raw, ok := commandLine.Raw.(*collections.OrderedMap[string, any]); ok {
			commandLineRaw = &collections.OrderedMap[string, any]{}
			commandLineRaw.Set("compilerOptions", raw)
		}
		parsed, errs := tsoptions.GetParsedCommandLineOfConfigFile(configFileName, options, commandLineRaw, sys, extendedConfigCache)
		if len(errs) != 0 {
			for _, e := range errs {
				reportDiagnostic(e)
			}
			return int(tsc.ExitStatusDiagnosticsPresent_OutputsGenerated), nil
		}
		config = parsed
		reportDiagnostic = tsc.CreateDiagnosticReporter(sys, sys.Writer(), locale, options)
	}
	effective := config.CompilerOptions()
	if effective.Watch.IsTrue() || effective.IsIncremental() || effective.GenerateTrace != "" ||
		effective.Diagnostics.IsTrue() || effective.ExtendedDiagnostics.IsTrue() {
		return 0, ErrUnsupported
	}

	host := &sharedHost{
		CompilerHost: compiler.NewCachedFSCompilerHost(sys.GetCurrentDirectory(), sys.FS(), sys.DefaultLibraryPath(),
			extendedConfigCache, c.tracer(sys.Writer(), config.Locale())),
		files: &c.files,
		done:  ctx.Done(),
	}
	program := compiler.NewProgram(compiler.ProgramOptions{Config: config, Host: host})
	result, _ := tsc.EmitAndReportStatistics(tsc.EmitInput{
		Sys:                sys,
		ProgramLike:        unbound{program},
		Program:            program,
		Config:             config,
		ReportDiagnostic:   reportDiagnostic,
		ReportErrorSummary: tsc.CreateReportErrorSummary(sys, locale, effective),
		Writer:             sys.Writer(),
		CompileTimes:       &tsc.CompileTimes{},
	})
	return int(result.Status), nil
}

// tracer is tsc.GetTraceWithWriterFromSys's trace, each message formatted
// as diagnostics.Format does it without its regular expression. tsgo traces
// from one goroutine, so one buffer holds every line.
func (c *Compiler) tracer(w io.Writer, loc locale.Locale) func(*diagnostics.Message, ...any) {
	var line []byte
	return func(msg *diagnostics.Message, args ...any) {
		key := templateKey{msg, loc}
		template, ok := c.templates.Load(key)
		if !ok {
			template, _ = c.templates.LoadOrStore(key, diagnostics.Localize(loc, msg, ""))
		}
		line, ok = appendFormat(line[:0], template, diagnostics.StringifyArgs(args))
		if !ok {
			line = append(line[:0], msg.Localize(loc, args...)...)
		}
		line = append(line, '\n')
		_, _ = w.Write(line)
	}
}

type templateKey struct {
	msg    *diagnostics.Message
	locale locale.Locale
}

// appendFormat appends template with each {N} replaced by args[N], as
// diagnostics.Format does; ok is false where Format would panic, for it to do so.
func appendFormat(dst []byte, template string, args []string) ([]byte, bool) {
	if len(args) == 0 {
		return append(dst, template...), true
	}
	for {
		open := strings.IndexByte(template, '{')
		if open < 0 {
			return append(dst, template...), true
		}
		digits := open + 1
		for digits < len(template) && '0' <= template[digits] && template[digits] <= '9' {
			digits++
		}
		if digits == open+1 || digits == len(template) || template[digits] != '}' {
			dst = append(dst, template[:open+1]...)
			template = template[open+1:]
			continue
		}
		index, err := strconv.Atoi(template[open+1 : digits])
		if err != nil || index >= len(args) {
			return dst, false
		}
		dst = append(dst, template[:open]...)
		dst = append(dst, strings.ToValidUTF8(args[index], "\uFFFD")...)
		template = template[digits+1:]
	}
}

// unbound skips binding, whose diagnostics a --listFilesOnly run discards.
type unbound struct {
	compiler.ProgramLike
}

func (unbound) GetBindDiagnostics(context.Context, *ast.SourceFile) []*ast.Diagnostic {
	return nil
}

// sharedHost parses through the Compiler's files: a file's tree depends only
// on its text and its parse options, as the language server's parse cache has it.
type sharedHost struct {
	compiler.CompilerHost
	files *collections.SyncMap[fileKey, *parsedFile]
	done  <-chan struct{}
}

func (h *sharedHost) GetSourceFile(options ast.SourceFileParseOptions) *ast.SourceFile {
	select {
	case <-h.done:
		return nil
	default:
	}
	text, ok := h.FS().ReadFile(options.FileName)
	if !ok {
		return nil
	}
	kind := core.GetScriptKindFromFileName(options.FileName)
	entry, _ := h.files.LoadOrStore(fileKey{options, kind}, &parsedFile{})
	entry.once.Do(func() {
		entry.text, entry.file = text, parser.ParseSourceFile(options, text, kind)
	})
	if entry.text == text {
		return entry.file
	}
	// The file changed since a listing parsed it: the next listings read this text.
	fresh := &parsedFile{}
	fresh.once.Do(func() {
		fresh.text, fresh.file = text, parser.ParseSourceFile(options, text, kind)
	})
	h.files.Store(fileKey{options, kind}, fresh)
	return fresh.file
}

// system is tsgo's own for a release build, whose lib files are on disk, with
// stdout not a terminal.
type system struct {
	stdout io.Writer
	fs     vfs.FS
	libDir string
	cwd    string
	start  time.Time
}

func (s *system) Writer() io.Writer                         { return s.stdout }
func (s *system) FS() vfs.FS                                { return s.fs }
func (s *system) DefaultLibraryPath() string                { return s.libDir }
func (s *system) GetCurrentDirectory() string               { return s.cwd }
func (s *system) WriteOutputIsTTY() bool                    { return false }
func (s *system) GetWidthOfTerminal() int                   { return 0 }
func (s *system) GetEnvironmentVariable(name string) string { return os.Getenv(name) }
func (s *system) Now() time.Time                            { return time.Now() }
func (s *system) SinceStart() time.Duration                 { return time.Since(s.start) }
