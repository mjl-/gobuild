package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mjl-/goreleases"
)

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
		//	{"android", "386"},
		//	{"android", "amd64"},
		//	{"android", "arm"},
		//	{"android", "arm64"},
		{"darwin", "386"},
		{"darwin", "amd64"},
		//	{"darwin", "arm"},
		//	{"darwin", "arm64"},
		{"dragonfly", "amd64"},
		{"freebsd", "386"},
		{"freebsd", "amd64"},
		{"freebsd", "arm"},
		{"freebsd", "arm64"},
		{"illumos", "amd64"},
		{"js", "wasm"},
		{"linux", "386"},
		{"linux", "amd64"},
		{"linux", "arm"},
		{"linux", "arm64"},
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
		{"plan9", "386"},
		{"plan9", "amd64"},
		{"plan9", "arm"},
		{"solaris", "amd64"},
		{"windows", "386"},
		{"windows", "amd64"},
		{"windows", "arm"},
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
	sort.Slice(n, func(i, j int) bool {
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
	supportedList []string  // List of latest supported releases, from https://golang.org/dl/?mode=json.
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
	for _, e := range sdk.supportedList {
		if e == goversion {
			return true
		}
	}
	return false
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

func ensureMostRecentSDK() (string, error) {
	supported, _ := installedSDK()
	if len(supported) == 0 {
		return "", fmt.Errorf("%w: no supported go versions", errServer)
	}
	if err := ensureSDK(supported[0]); err != nil {
		return "", err
	}
	return supported[0], nil
}

func installedSDK() (supported []string, remainingAvailable []string) {
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
			log.Printf("listing supported go releases: %v", err)
		} else {
			sdk.supportedList = []string{}
			for _, rel := range rels {
				sdk.supportedList = append(sdk.supportedList, rel.Version)
			}
			sdkUpdateInstalledList()
		}
	}
	supported = sdk.supportedList
	remainingAvailable = sdk.installedList
	return
}

var errBadGoversion = errors.New("bad goversion")

func ensureSDK(goversion string) error {
	// Reproducible builds work from go1.13 onwards. Refuse earlier versions.
	if !strings.HasPrefix(goversion, "go") {
		return fmt.Errorf(`%w: must start with "go"`, errBadGoversion)
	}
	if strings.HasPrefix(goversion, "go1") {
		// Handle "rc" and "beta" versions by stripping those parts.
		v := strings.SplitN(goversion, "rc", 2)[0]
		v = strings.SplitN(v, "beta", 2)[0]
		if len(v) < 4 || !strings.HasPrefix(v, "go1.") {
			return fmt.Errorf("%w: old version, must be >=go1.13", errBadGoversion)
		}
		if num, err := strconv.ParseInt(strings.Split(v[4:], ".")[0], 10, 64); err != nil || num < 13 {
			return fmt.Errorf("%w: bad version, must be >=go1.13", errBadGoversion)
		}
	}

	// See if this is an SDK we know we have installed.
	sdk.Lock()
	if _, ok := sdk.installed[goversion]; ok {
		sdk.Unlock()
		return nil
	}
	sdk.Unlock()

	// Not installed yet. Let's see if we've fetched it before. If we tried and failed
	// before, we won't try again (during the lifetime of this process). If another
	// goroutine has installed it while we were waiting on the lock, we know this by
	// the presence of an entry in status, without an error.
	sdk.fetch.Lock()
	defer sdk.fetch.Unlock()
	if err, ok := sdk.fetch.status[goversion]; ok {
		return err
	}

	rels, err := goreleases.ListAll()
	if err != nil {
		err = fmt.Errorf("%w: listing known releases: %v", errRemote, err)
		sdk.fetch.status[goversion] = err
		return err
	}
	for _, rel := range rels {
		if rel.Version != goversion {
			continue
		}

		f, err := goreleases.FindFile(rel, runtime.GOOS, runtime.GOARCH, "archive")
		if err != nil {
			err = fmt.Errorf("%w: finding release file: %v", errServer, err)
			sdk.fetch.status[goversion] = err
			return err
		}
		tmpdir, err := os.MkdirTemp(config.SDKDir, "tmpsdk")
		if err != nil {
			err = fmt.Errorf("%w: making tempdir for sdk: %v", errServer, err)
			sdk.fetch.status[goversion] = err
			return err
		}
		defer os.RemoveAll(tmpdir)

		log.Printf("fetching sdk for %v", goversion)

		if err := goreleases.Fetch(f, tmpdir, nil); err != nil {
			err = fmt.Errorf("%w: installing sdk: %v", errServer, err)
			sdk.fetch.status[goversion] = err
			return err
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
			return err
		} else if err := os.Rename(filepath.Join(tmpdir, "go"), filepath.Join(config.SDKDir, goversion)); err != nil {
			err = fmt.Errorf("%w: putting sdk in place: %v", errServer, err)
			sdk.fetch.status[goversion] = err
			return err
		} else {
			sdk.fetch.status[goversion] = nil

			sdk.Lock()
			defer sdk.Unlock()
			sdk.installed[goversion] = struct{}{}
			sdkUpdateInstalledList()
		}
		return nil
	}

	// Release not found. It may be a future release. Don't mark it as
	// tried-and-failed.
	// We may want to ratelimit how often we ask...
	return fmt.Errorf("%w: no such version", errBadGoversion)
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
	cmd := makeCommand(goproxy, emptyDir, cgo, moreEnv, gobin, "build", "-trimpath", "std")
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("go build std: %v\n%s", err, output)
		return err
	}
	if err := os.WriteFile(primedPath, []byte{}, 0666); err != nil {
		log.Printf("writefile %s: %v", primedPath, err)
		return err
	}
	return nil
}
