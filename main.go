package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

const version = "0.9.5-snapshot"

type gobco struct {
	firstTime   bool
	listAll     bool
	immediately bool
	keep        bool
	verbose     bool
	coverTest   bool

	goTestOpts []string
	args       []argument

	statsFilename string

	exitCode int

	runenv
	buildEnv
}

func newGobco(stdout io.Writer, stderr io.Writer) *gobco {
	var g gobco
	g.runenv.init(stdout, stderr)
	g.buildEnv.init(&g.runenv)
	return &g
}

func (g *gobco) parseCommandLine(argv []string) {
	var help, ver bool

	flags := flag.NewFlagSet(filepath.Base(argv[0]), flag.ContinueOnError)
	flags.BoolVar(&g.firstTime, "first-time", false,
		"print each condition to stderr when it is reached the first time")
	flags.BoolVar(&help, "help", false,
		"print the available command line options")
	flags.BoolVar(&g.immediately, "immediately", false,
		"persist the coverage immediately at each check point")
	flags.BoolVar(&g.keep, "keep", false,
		"don't remove the temporary working directory")
	flags.BoolVar(&g.listAll, "list-all", false,
		"at finish, print also those conditions that are fully covered")
	flags.StringVar(&g.statsFilename, "stats", "",
		"load and persist the JSON coverage data to this `file`")
	flags.Var(newSliceFlag(&g.goTestOpts), "test",
		"pass the `option` to \"go test\", such as -vet=off")
	flags.BoolVar(&g.verbose, "verbose", false,
		"show progress messages")
	flags.BoolVar(&g.coverTest, "cover-test", false,
		"cover the test code as well")
	flags.BoolVar(&ver, "version", false,
		"print the gobco version")

	flags.SetOutput(g.stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "usage: %s [options] package...\n", flags.Name())
		flags.PrintDefaults()
		g.exitCode = 2
	}

	err := flags.Parse(argv[1:])
	if g.exitCode != 0 {
		exit(g.exitCode)
	}
	g.ok(err)

	if help {
		flags.SetOutput(g.stdout)
		flags.Usage()
		exit(0)
	}

	if ver {
		g.outf("%s\n", version)
		exit(0)
	}

	args := flags.Args()
	if len(args) == 0 {
		args = []string{"."}
	}

	if len(args) > 1 {
		panic("gobco: checking multiple packages doesn't work yet")
	}

	for _, arg := range args {
		st, err := os.Stat(arg)
		dir := err == nil && st.IsDir()

		rel := g.rel(arg)
		g.args = append(g.args, argument{arg, rel, dir})
	}
}

// rel returns the path of the argument, relative to the current $GOPATH/src,
// using forward slashes.
func (g *gobco) rel(arg string) string {
	gopath := strings.Split(os.Getenv("GOPATH"), string(filepath.ListSeparator))[0]
	if gopath == "" {
		home, err := os.UserHomeDir()
		g.ok(err)
		gopath = filepath.Join(home, "go")
	}

	abs, err := filepath.Abs(arg)
	g.ok(err)

	gopathSrc := filepath.Join(gopath, "src")
	rel, err := filepath.Rel(gopathSrc, abs)
	g.ok(err)

	if strings.HasPrefix(rel, "..") {
		g.ok(fmt.Errorf("argument %q must be inside %q", arg, gopathSrc))
	}

	return filepath.ToSlash(rel)
}

// prepareTmp copies the source files to the temporary directory.
//
// Some of these files will later be overwritten by gobco.instrumenter.
func (g *gobco) prepareTmp() {
	if g.statsFilename != "" {
		var err error
		g.statsFilename, err = filepath.Abs(g.statsFilename)
		g.ok(err)
	} else {
		g.statsFilename = filepath.Join(g.tmpdir, "gobco-counts.json")
	}

	// TODO: Research how "package/..." is handled by other go commands.
	for _, arg := range g.args {
		g.prepareTmpDir(arg)
	}
}

func (g *gobco) prepareTmpDir(arg argument) {
	srcDir := arg.srcDir()
	dstDir := g.fileSrc(arg.tmpDir())
	g.ok(copyDir(srcDir, dstDir))
}

func (g *gobco) instrument() {
	var in instrumenter
	in.firstTime = g.firstTime
	in.immediately = g.immediately
	in.listAll = g.listAll
	in.coverTest = g.coverTest

	for _, arg := range g.args {
		dir := g.fileSrc(arg.tmpDir())
		base := arg.base()
		in.instrument(dir, base)
		g.verbosef("Instrumented %s to %s", arg.argName, arg.tmpName)
	}
}

func (g *gobco) runGoTest() {
	g.exitCode = goTest{}.run(g.args, g.goTestOpts, g.verbose, g.statsFilename, &g.buildEnv)
}

func (g *gobco) cleanUp() {
	if g.keep {
		g.errf("\n")
		g.errf("the temporary files are in %s\n", g.tmpdir)
	} else {
		err := os.RemoveAll(g.tmpdir)
		if err != nil {
			g.verbosef("%s", err)
		}
	}
}

func (g *gobco) printOutput() {
	conds := g.load(g.statsFilename)

	cnt := 0
	for _, c := range conds {
		if c.TrueCount > 0 {
			cnt++
		}
		if c.FalseCount > 0 {
			cnt++
		}
	}

	g.outf("\n")
	g.outf("Branch coverage: %d/%d\n", cnt, len(conds)*2)

	for _, cond := range conds {
		g.printCond(cond)
	}
}

func (g *gobco) load(filename string) []condition {
	file, err := os.Open(filename)
	g.ok(err)

	defer func() {
		closeErr := file.Close()
		g.ok(closeErr)
	}()

	var data []condition
	decoder := json.NewDecoder(bufio.NewReader(file))
	decoder.DisallowUnknownFields()
	g.ok(decoder.Decode(&data))

	return data
}

func (g *gobco) printCond(cond condition) {

	trueCount := cond.TrueCount
	falseCount := cond.FalseCount
	start := cond.Start
	code := cond.Code

	if !g.listAll && trueCount > 0 && falseCount > 0 {
		return
	}

	capped := func(count int) int {
		if count > 1 {
			return 2
		}
		if count == 1 {
			return 1
		}
		return 0
	}

	switch 3*capped(trueCount) + capped(falseCount) {
	case 0:
		g.outf("%s: condition %q was never evaluated\n",
			start, code)
	case 1:
		g.outf("%s: condition %q was once false but never true\n",
			start, code)
	case 2:
		g.outf("%s: condition %q was %d times false but never true\n",
			start, code, falseCount)
	case 3:
		g.outf("%s: condition %q was once true but never false\n",
			start, code)
	case 4:
		g.outf("%s: condition %q was once true and once false\n",
			start, code)
	case 5:
		g.outf("%s: condition %q was once true and %d times false\n",
			start, code, falseCount)
	case 6:
		g.outf("%s: condition %q was %d times true but never false\n",
			start, code, trueCount)
	case 7:
		g.outf("%s: condition %q was %d times true and once false\n",
			start, code, trueCount)
	case 8:
		g.outf("%s: condition %q was %d times true and %d times false\n",
			start, code, trueCount, falseCount)
	}
}

type goTest struct{}

func (t goTest) run(
	arguments []argument,
	extraArgs []string,
	verbose bool,
	statsFilename string,
	e *buildEnv,
) int {
	args := t.args(arguments, verbose, extraArgs)
	goTest := exec.Command("go", args[1:]...)
	goTest.Stdout = e.stdout
	goTest.Stderr = e.stderr
	goTest.Dir = filepath.Join(e.tmpdir, "src")
	goTest.Env = t.env(e.tmpdir, statsFilename)

	cmdline := strings.Join(args, " ")
	e.verbosef("Running %q in %q", cmdline, goTest.Dir)

	err := goTest.Run()
	if err != nil {
		e.errf("%s\n", err)
		return 1
	} else {
		e.verbosef("Finished %s", cmdline)
		return 0
	}
}

func (goTest) args(
	arguments []argument,
	verbose bool,
	extraArgs []string,
) []string {
	// The -v is necessary to produce any output at all.
	// Without it, most of the log output is suppressed.
	args := []string{"go", "test"}

	if verbose {
		args = append(args, "-v")
	}

	// Work around test result caching which does not apply anyway,
	// since the instrumented files are written to a new directory
	// each time.
	//
	// Without this option, "go test" sometimes needs twice the time.
	args = append(args, "-test.count", "1")

	seenDirs := make(map[string]bool)
	for _, arg := range arguments {
		dir := arg.tmpDir()

		if !seenDirs[dir] {
			args = append(args, dir)
			seenDirs[dir] = true
		}
	}

	args = append(args, extraArgs...)

	return args
}

func (goTest) env(tmpdir string, statsFilename string) []string {
	gopath := fmt.Sprintf("%s%c%s", tmpdir, filepath.ListSeparator, os.Getenv("GOPATH"))

	var env []string
	env = append(env, os.Environ()...)
	env = append(env, "GOPATH="+gopath)
	env = append(env, "GOBCO_STATS="+statsFilename)

	return env
}

type buildEnv struct {
	tmpdir string
	*runenv
}

func (e *buildEnv) init(r *runenv) {
	var rnd [16]byte
	_, err := io.ReadFull(rand.Reader, rnd[:])
	r.ok(err)

	tmpdir := filepath.Join(os.TempDir(), fmt.Sprintf("gobco-%x", rnd))

	r.ok(os.MkdirAll(tmpdir, 0777))

	r.verbosef("The temporary working directory is %s", tmpdir)

	*e = buildEnv{tmpdir, r}
}

// file returns the absolute path to the given path, which is interpreted
// relative to $GOROOT/src. The result uses native slashes.
func (e *buildEnv) fileSrc(rel string) string {
	return filepath.Join(e.tmpdir, "src", filepath.FromSlash(rel))
}

func (e *buildEnv) file(rel string) string {
	return filepath.Join(e.tmpdir, filepath.FromSlash(rel))
}

// runenv provides basic logging and error checking.
type runenv struct {
	stdout  io.Writer
	stderr  io.Writer
	verbose bool
}

func (r *runenv) init(stdout io.Writer, stderr io.Writer) {
	r.stdout = stdout
	r.stderr = stderr
}

func (r *runenv) ok(err error) {
	if err != nil {
		r.errf("%s\n", err)
		exit(1)
	}
}

func (r *runenv) outf(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(r.stdout, format, args...)
}

func (r *runenv) errf(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(r.stderr, format, args...)
}

func (r *runenv) verbosef(format string, args ...interface{}) {
	if r.verbose {
		r.errf(format+"\n", args...)
	}
}

type argument struct {
	// from the command line, using '/' as separator
	argName string

	// relative to the temporary $GOPATH/src, using forward slashes.
	tmpName string

	isDir bool
}

func (a *argument) base() string {
	if a.isDir {
		return ""
	}
	return path.Base(a.argName)
}

func (a *argument) srcDir() string {
	if a.isDir {
		return a.argName
	}
	return path.Dir(a.argName)
}

// tmpDir returns the directory where the files from this argument should be
// copied, to be instrumented there.
func (a *argument) tmpDir() string {
	if a.isDir {
		return a.tmpName
	}
	return path.Dir(a.tmpName)
}

type condition struct {
	Start      string
	Code       string
	TrueCount  int
	FalseCount int
}

var exit = os.Exit

func gobcoMain(stdout, stderr io.Writer, args ...string) {
	g := newGobco(stdout, stderr)
	g.parseCommandLine(args)
	g.prepareTmp()
	g.instrument()
	g.runGoTest()
	g.printOutput()
	g.cleanUp()
	exit(g.exitCode)
}

func main() {
	gobcoMain(os.Stdout, os.Stderr, os.Args...)
}
