package main

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mjl-/goreleases"
	"slices"
)

type goVersion struct {
	major, minor, patch int    // go1.19 would be {1,19,0}.
	more                string // "rc1" for go1.19rc1.
}

func (v goVersion) String() string {
	s := fmt.Sprintf("go%d.%d", v.major, v.minor)
	// Per go1.21, the first release is go1.21.0.
	if v.patch > 0 || v.major == 1 && v.minor >= 21 {
		s += fmt.Sprintf(".%d", v.patch)
	}
	s += v.more
	return s
}

// simple number for comparison. 8 bits should be fine.
// we disregard "more" for comparisons.
func (v goVersion) num() int {
	return v.major<<16 | v.minor<<8 | v.patch<<0
}

// If not nil, we don't allow using this or newer Go toolchains.
var sdkVersionStop *goVersion

type target struct {
	Goos   string
	Goarch string
}

func (t target) osarch() string {
	return t.Goos + "/" + t.Goarch
}

// List of targets from "go tool dist list", bound to be out of date here; should probably generate on startup, or when we get the first sdk installed.
// Android and darwin/arm* cannot build on my linux/amd64 machine.
// Note: list will be sorted after startup by readRecentBuilds, most used first.
type xtargets struct {
	sync.Mutex
	use      map[string]int // Used for popularity, and for validating build requests.
	totalUse int
	list     []target

	// Targets available for buildling. Use without lock.
	available map[string]struct{}
}

var targets = &xtargets{
	sync.Mutex{},
	map[string]int{},
	0,
	[]target{
		{"aix", "ppc64"},
		{"android", "386"},
		{"android", "amd64"},
		{"android", "arm"},
		{"android", "arm64"},
		{"darwin", "386"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
		{"dragonfly", "amd64"},
		{"freebsd", "386"},
		{"freebsd", "amd64"},
		{"freebsd", "arm"},
		{"freebsd", "arm64"},
		{"freebsd", "riscv64"},
		{"illumos", "amd64"},
		{"ios", "amd64"},
		{"ios", "arm64"},
		{"js", "wasm"},
		{"linux", "386"},
		{"linux", "amd64"},
		{"linux", "arm"},
		{"linux", "arm64"},
		{"linux", "loong64"},
		{"linux", "mips"},
		{"linux", "mips64"},
		{"linux", "mips64le"},
		{"linux", "mipsle"},
		{"linux", "ppc64"},
		{"linux", "ppc64le"},
		{"linux", "riscv64"},
		{"linux", "s390x"},
		{"netbsd", "386"},
		{"netbsd", "amd64"},
		{"netbsd", "arm"},
		{"netbsd", "arm64"},
		{"openbsd", "386"},
		{"openbsd", "amd64"},
		{"openbsd", "arm"},
		{"openbsd", "arm64"},
		{"openbsd", "ppc64"},
		{"openbsd", "riscv64"},
		{"plan9", "386"},
		{"plan9", "amd64"},
		{"plan9", "arm"},
		{"solaris", "amd64"},
		// {"wasip1", "wasm"},
		{"windows", "386"},
		{"windows", "amd64"},
		{"windows", "arm"},
		{"windows", "arm64"},
	},
	map[string]struct{}{},
}

func init() {
	for _, t := range targets.list {
		targets.use[t.osarch()] = 0
		targets.available[t.osarch()] = struct{}{}
	}
}

func (t *xtargets) get() []target {
	t.Lock()
	defer t.Unlock()
	return t.list
}

func (t *xtargets) valid(target string) bool {
	t.Lock()
	defer t.Unlock()
	_, ok := t.use[target]
	return ok
}

// must be called with lock held.
func (t *xtargets) sort() {
	n := make([]target, len(t.list))
	copy(n, t.list)
	sort.SliceStable(n, func(i, j int) bool {
		return t.use[n[i].osarch()] > t.use[n[j].osarch()]
	})
	t.list = n
}

func (t *xtargets) increase(target string) {
	t.Lock()
	defer t.Unlock()
	t.use[target]++
	t.totalUse++
	if t.totalUse <= 32 || t.totalUse%32 == 0 {
		t.sort()
	}
}

var sdk struct {
	sync.Mutex
	installed     map[string]struct{}
	lastSupported time.Time // When last supported list was fetched. We fetch at most once per hour.
	supportedList []string  // List of latest supported releases, from https://go.dev/dl/?mode=json.
	installedList []string  // List of all other installed releases.

	fetch struct {
		sync.Mutex
		status map[string]error
	}
}

func initSDK() {
	sdk.installed = map[string]struct{}{}
	l, err := os.ReadDir(config.SDKDir)
	if err != nil {
		log.Fatalf("readdir sdk: %v", err)
	}
	for _, e := range l {
		if strings.HasPrefix(e.Name(), "go") {
			sdk.installed[e.Name()] = struct{}{}
		}
	}
	sdkUpdateInstalledList()

	sdk.fetch.status = map[string]error{}
}

// Lock must be held by calling.
func sdkIsSupported(goversion string) bool {
	return slices.Contains(sdk.supportedList, goversion)
}

// Lock must be held by calling.
func sdkUpdateInstalledList() {
	l := []string{}
	for goversion := range sdk.installed {
		if !sdkIsSupported(goversion) {
			l = append(l, goversion)
		}
	}
	sort.Slice(l, func(i, j int) bool {
		return l[j] < l[i]
	})
	sdk.installedList = l
}

func ensureMostRecentSDK() (goVersion, error) {
	newestAllowed, _, _ := listSDK()
	if newestAllowed == "" {
		return goVersion{}, fmt.Errorf("%w: no supported go versions", errServer)
	}
	if gv, err := ensureSDK(newestAllowed); err != nil {
		return goVersion{}, err
	} else {
		return gv, nil
	}
}

func listSDK() (newestAllowed string, supported []string, remainingAvailable []string) {
	now := time.Now()
	sdk.Lock()
	defer sdk.Unlock() // note: we unlock and relock below!

	if now.Sub(sdk.lastSupported) > time.Hour {
		// Don't hold lock while requesting. Don't let others make the same request.
		sdk.lastSupported = now
		sdk.Unlock()
		// todo: set a (low) timeout on the request
		rels, err := goreleases.ListSupported()
		sdk.Lock()

		if err != nil {
			slog.Error("listing supported go releases", "err", err)
		} else {
			sdk.supportedList = []string{}
			for _, rel := range rels {
				sdk.supportedList = append(sdk.supportedList, rel.Version)
				if newestAllowed != "" {
					continue
				}
				if sdkVersionStop == nil {
					newestAllowed = rel.Version
				} else if gv, err := parseGoVersion(rel.Version); err != nil {
					slog.Error("parsing go version from listing released go toolchain", "goversion", rel.Version, "err", err)
				} else if gv.num() < sdkVersionStop.num() {
					newestAllowed = gv.String()
				}
			}
			sdkUpdateInstalledList()
		}
	}
	supported = sdk.supportedList
	remainingAvailable = sdk.installedList
	// Better to return a toolchain that will result in later ensure failure, than to claim there is no toolchain.
	if newestAllowed == "" && len(supported) > 0 {
		newestAllowed = supported[0]
	}
	return
}

var errBadGoversion = errors.New("bad goversion")

func parseGoVersion(s string) (goVersion, error) {
	r := goVersion{major: 1}

	// Reproducible builds work from go1.13 onwards. Refuse earlier versions.
	if !strings.HasPrefix(s, "go1.") {
		return r, fmt.Errorf(`must start with "go1."`)
	}
	s = strings.TrimPrefix(s, "go1.")

	// Handle "rc" and "beta" versions by stripping those parts.
	if t := strings.SplitN(s, "rc", 2); len(t) == 2 {
		r.more = "rc" + t[1]
		s = t[0]
	} else if t := strings.SplitN(s, "beta", 2); len(t) == 2 {
		r.more = "beta" + t[1]
		s = t[0]
	}
	t := strings.Split(s, ".")
	if len(t) != 1 && len(t) != 2 {
		return r, fmt.Errorf("need 0 or 1 dot")
	}
	minor, err := strconv.ParseInt(t[0], 10, 32)
	if err != nil {
		return r, fmt.Errorf(`invalid number after "go1."`)
	}
	r.minor = int(minor)
	if len(t) == 2 {
		patch, err := strconv.ParseInt(t[1], 10, 32)
		if err != nil {
			return r, fmt.Errorf("invalid last number")
		}
		r.patch = int(patch)
	}
	if r.minor < 13 {
		return r, fmt.Errorf("old version, must be >=go1.13")
	}

	return r, nil
}

func ensureSDK(goversion string) (goVersion, error) {
	gv, err := parseGoVersion(goversion)
	if err != nil {
		return goVersion{}, fmt.Errorf("%w: %s", errBadGoversion, err)
	}
	if sdkVersionStop != nil && gv.num() >= sdkVersionStop.num() {
		return goVersion{}, fmt.Errorf("%w: version equal or newer than %s not allowed by config", errBadGoversion, sdkVersionStop.String())
	}

	// See if this is an SDK we know we have installed.
	sdk.Lock()
	if _, ok := sdk.installed[goversion]; ok {
		sdk.Unlock()
		return gv, nil
	}
	sdk.Unlock()

	// Not installed yet. Let's see if we've fetched it before. If we tried and failed
	// before, we won't try again (during the lifetime of this process). If another
	// goroutine has installed it while we were waiting on the lock, we know this by
	// the presence of an entry in status, without an error.
	sdk.fetch.Lock()
	defer sdk.fetch.Unlock()
	if err, ok := sdk.fetch.status[goversion]; ok {
		return gv, err
	}

	rels, err := goreleases.ListAll()
	if err != nil {
		err = fmt.Errorf("%w: listing known releases: %v", errRemote, err)
		sdk.fetch.status[goversion] = err
		return goVersion{}, err
	}
	for _, rel := range rels {
		if rel.Version != goversion {
			continue
		}

		f, err := goreleases.FindFile(rel, runtime.GOOS, runtime.GOARCH, "archive")
		if err != nil {
			err = fmt.Errorf("%w: finding release file: %v", errServer, err)
			sdk.fetch.status[goversion] = err
			return goVersion{}, err
		}
		tmpdir, err := os.MkdirTemp(config.SDKDir, "tmpsdk")
		if err != nil {
			err = fmt.Errorf("%w: making tempdir for sdk: %v", errServer, err)
			sdk.fetch.status[goversion] = err
			return goVersion{}, err
		}
		defer os.RemoveAll(tmpdir)

		slog.Info("fetching sdk", "goversion", goversion)

		if err := goreleases.Fetch(f, tmpdir, nil); err != nil {
			err = fmt.Errorf("%w: installing sdk: %v", errServer, err)
			sdk.fetch.status[goversion] = err
			return goVersion{}, err
		}
		gobin := filepath.Join(tmpdir, "go", "bin", "go"+goexe())
		if !filepath.IsAbs(gobin) {
			gobin = filepath.Join(workdir, gobin)
		}
		// Priming here is not strictly necessary, but it's a good check the toolchain
		// works, and prevents multiple immediately builds from doing this same work
		// concurrently.
		if err := ensurePrimedBuildCache(gobin, runtime.GOOS, runtime.GOARCH, goversion); err != nil {
			err = fmt.Errorf("%w: priming build cache: %v", errServer, err)
			sdk.fetch.status[goversion] = err
			return goVersion{}, err
		} else if err := os.Rename(filepath.Join(tmpdir, "go"), filepath.Join(config.SDKDir, goversion)); err != nil {
			err = fmt.Errorf("%w: putting sdk in place: %v", errServer, err)
			sdk.fetch.status[goversion] = err
			return goVersion{}, err
		} else {
			sdk.fetch.status[goversion] = nil

			sdk.Lock()
			defer sdk.Unlock()
			sdk.installed[goversion] = struct{}{}
			sdkUpdateInstalledList()
		}
		return gv, nil
	}

	// Release not found. It may be a future release. Don't mark it as
	// tried-and-failed.
	// We may want to ratelimit how often we ask...
	return goVersion{}, fmt.Errorf("%w: no such version", errBadGoversion)
}

func goexe() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// We compile the standard library for an architecture explicitly. This lets us do
// other builds with a read-only cache. This lets builds reuse a good part of their
// builds, without getting a big cache.
func ensurePrimedBuildCache(gobin, goos, goarch, goversion string) error {
	// Disable for now. We are not using the toolchain. This just makes
	// fetching a new toolchain slow, and only takes up space. Should
	// enable it again when we want to use it.
	if true {
		return nil
	}

	primedPath := filepath.Join(homedir, ".cache", "gobuild", fmt.Sprintf("%s-%s-%s.primed", goos, goarch, goversion))
	if _, err := os.Stat(primedPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	os.MkdirAll(filepath.Dir(primedPath), 0777) // errors ignored

	goproxy := false
	cgo := false
	moreEnv := []string{
		"GOOS=" + goos,
		"GOARCH=" + goarch,
	}
	cmd := makeCommand(goversion, goproxy, emptyDir, cgo, moreEnv, gobin, "build", "-trimpath", "std")
	if output, err := cmd.CombinedOutput(); err != nil {
		slog.Error("go build std", "err", err, "output", output)
		return err
	}
	if err := os.WriteFile(primedPath, []byte{}, 0666); err != nil {
		slog.Error("writefile for primed path", "path", primedPath, "err", err)
		return err
	}
	return nil
}
