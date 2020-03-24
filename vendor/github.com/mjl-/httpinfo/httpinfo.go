/*
Package httpinfo provides an HTTP handler serving build information (compiler used, modules included, VCS versions) about a running service.

Example usage:

	// Variables with versions for your service. Set them at compile time like so:
	//	go build -ldflags "-X main.vcsCommitHash=${COMMITHASH} -X main.vcsTag=${TAG} -X main.vcsBranch=${BRANCH} -X main.version=${VERSION}"
	var (
		version = "dev"
		vcsCommitHash = ""
		vcsTag = ""
		vcsBranch = ""
	)

	func init() {
		// Since we set the version variables with ldflags -X, we cannot read them in the vars section.
		// So we combine them into a CodeVersion during init, and add the handler while we're at it.
		info := httpinfo.CodeVersion{
			CommitHash: vcsCommitHash,
			Tag:        vcsTag,
			Branch:     vcsBranch,
			Full:       version,
		}
		http.Handle("/info", httpinfo.NewHandler(info, nil))
	}
*/
package httpinfo

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

// Info is returned by the handler encoded as JSON.
type Info struct {
	BuildInfo *debug.BuildInfo // Modules used in the service.
	Time      time.Time        // Current time at server.
	Go        GoVersion        // Go compiler information.
	Hostname  string           // Hostname currently running on.
	Version   CodeVersion      // VCS version information for service.
	More      interface{}      // Other information you want to store.
}

// CodeVersion specifies the exact version of a VCS code repository.
type CodeVersion struct {
	CommitHash string // E.g. "d6d03be3edea736a79d925c736c1dfc0185a3bb9"
	Tag        string // E.g. "v0.1.2". Should be empty when the commit isn't tagged.
	Branch     string // E.g. "master".
	Full       string // Full description of version, may included tags and commits after tag. E.g. "0.1.2" for a cleanly tagged version. Or "0.2.0-15-gf247de4" for a tagged version plus changes and a short git commit hash.
}

// GoVersion specifies the Go compiler and runtime this application is running under.
type GoVersion struct {
	Compiler string // From runtime.Compiler.
	Arch     string // One of the GOARCH values.
	Os       string // One of the GOOS values.
	Version  string // E.g. "1.12.2".
}

type infoHandler struct {
	BuildInfo   *debug.BuildInfo
	GoVersion   GoVersion
	CodeVersion CodeVersion
	more        func() interface{}
}

// NewHandler returns a http handler serving service information for each GET request it receives.
// Buildinfo and Go version information is collected immediately.
// More should return a JSON-encodable value that will also be included. More can be nil. If set it is called for each request.
func NewHandler(version CodeVersion, more func() interface{}) http.Handler {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		log.Println("no buildinfo available for /info")
	}
	gov := GoVersion{
		Compiler: runtime.Compiler,
		Arch:     runtime.GOARCH,
		Os:       runtime.GOOS,
		Version:  runtime.Version(),
	}
	return infoHandler{bi, gov, version, more}
}

// ServeHTTP serves GET requests with a JSON-encoded Info.
func (h infoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var more interface{}
	if h.more != nil {
		more = h.more()
	}
	hostname, _ := os.Hostname()
	var info = Info{
		BuildInfo: h.BuildInfo,
		Time:      time.Now(),
		Go:        h.GoVersion,
		Hostname:  hostname,
		Version:   h.CodeVersion,
		More:      more,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err := json.NewEncoder(w).Encode(info)
	if err != nil {
		log.Printf("writing info: %s\n", err)
	}
}
