package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/mjl-/goreleases"
)

var status error

type target struct {
	Goos   string
	Goarch string
}

// by "go tool dist list", bound to be out of date here; should probably generate on startup, or when we get the first sdk installed
var targets = []target{
	{"aix", "ppc64"},
	{"android", "386"},
	{"android", "amd64"},
	{"android", "arm"},
	{"android", "arm64"},
	{"darwin", "386"},
	{"darwin", "amd64"},
	{"darwin", "arm"},
	{"darwin", "arm64"},
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
}

var sdk struct {
	sync.Mutex
	installed map[string]struct{}

	fetch struct {
		sync.Mutex
		status map[string]error
	}
}

func init() {
	sdk.installed = map[string]struct{}{}
	l, err := ioutil.ReadDir("sdk")
	if err != nil {
		log.Fatalf("readdir sdk: %v", err)
	}
	for _, e := range l {
		if strings.HasPrefix(e.Name(), "go") {
			sdk.installed[e.Name()] = struct{}{}
		}
	}

	sdk.fetch.status = map[string]error{}
}

func installedSDK() []string {
	l := []string{}
	sdk.Lock()
	for goversion := range sdk.installed {
		l = append(l, goversion)
	}
	sdk.Unlock()
	sort.Slice(l, func(i, j int) bool {
		return l[j] < l[i]
	})
	return l
}

func ensureSDK(goversion string) error {
	// Reproducible builds work from go1.13 onwards. Refuse earlier versions.
	if !strings.HasPrefix(goversion, "go") {
		return fmt.Errorf(`goversion must start with "go"`)
	}
	if strings.HasPrefix(goversion, "go1") {
		if len(goversion) < 4 || !strings.HasPrefix(goversion, "go1.") {
			return fmt.Errorf("old version, must be >=go1.13")
		}
		num, err := strconv.ParseInt(strings.Split(goversion[4:], ".")[0], 10, 64)
		if err != nil || num < 13 {
			return fmt.Errorf("bad version, must be >=go1.13")
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
	err, ok := sdk.fetch.status[goversion]
	if ok {
		return err
	}

	rels, err := goreleases.ListAll()
	if err != nil {
		err = fmt.Errorf("listing known releases: %v", err)
		sdk.fetch.status[goversion] = err
		return err
	}
	for _, rel := range rels {
		if rel.Version == goversion {
			f, err := goreleases.FindFile(rel, runtime.GOOS, runtime.GOARCH, "archive")
			if err != nil {
				err = fmt.Errorf("finding release file: %v", err)
				sdk.fetch.status[goversion] = err
				return err
			}
			tmpdir, err := ioutil.TempDir("sdk", "tmp-install")
			if err != nil {
				err = fmt.Errorf("making tempdir for sdk: %v", err)
				sdk.fetch.status[goversion] = err
				return err
			}
			defer func() {
				os.RemoveAll(tmpdir)
			}()
			err = goreleases.Fetch(f, tmpdir, nil)
			if err != nil {
				err = fmt.Errorf("installing sdk: %v", err)
				sdk.fetch.status[goversion] = err
				return err
			}
			err = os.Rename(tmpdir+"/go", "sdk/"+goversion)
			if err != nil {
				err = fmt.Errorf("putting sdk in place: %v", err)
			} else {
				sdk.Lock()
				defer sdk.Unlock()
				sdk.installed[goversion] = struct{}{}
			}
			sdk.fetch.status[goversion] = err
			return err
		}
	}

	// Release not found. It may be a future release. Don't mark it as
	// tried-and-failed. We may want to ratelimit how often we ask...
	return fmt.Errorf("goversion not found")
}
