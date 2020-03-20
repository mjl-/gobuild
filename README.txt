Gobuild builds reproducible binaries on demand.

Gobuild serves URL that specify what to compile: Go module, version, package path in the URL, Go toolchain version, GOOS, GOARCH.

Gobuild serves HTML pages with links to the downloadable binary, along with links to other versions and targets.

URLs are of the form:

	http://localhost:8000/x/<goos>-<goarch>-<goversion>/<module>@<version>/<path>/{,log,sha256,dl,<sum>}

Gobuild builds by executing commands like:

	cd $tmpdir
	GO111MODULE=on GOPROXY=https://proxy.golang.org go get $module@$version
	cd $gopath/pkg/mod/$module@$version/$path
	CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -o $tmpdir/$name -x -trimpath -ldflags -buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000

Results are stored in ./data/.
Builds are done with a home directory of ./home/.
Toolchains are in ./sdk/.

# Todo

- Do in-process coordination between goroutines so we don't do the same build for different requests.
- Keep a queue of builds that are being requested. If we don't have a module/version/package build ready, we return an HTML page saying the build will be created. The page connects with SSE to register the desire for the build, and to stay up to date on the build schedule. If we leave the page, your position in the queue is dropped. We can perhaps do runtime.NumCPU()+1 builds at the same time.

- Should not strip entire buildid. I think it was needed in the past. Might need rules for some older Go versions that do need it.

- Create a transparency log with builds.

- Write tests.
- Cache responses from goproxy?
- Handle special characters in modules/versions/package paths.
