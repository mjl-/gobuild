Gobuild builds reproducible binaries on demand.

Gobuild serves URL that specify what to compile: Go module, version,
package path in the URL, Go toolchain version, GOOS, GOARCH.

Gobuild serves HTML pages with links to the downloadable binary, along
with links to other module versions, go versions and build targets.

Gobuild will automatically download new Go toolchains (SDKs).

URLs are of the form:

	http://localhost:8000/x/<goos>-<goarch>-<goversion>/<module>@<version>/<path>/{,log,sha256,dl,<sum>}

Gobuild builds by executing commands like:

	cd $tmpdir
	GO111MODULE=on GOPROXY=https://proxy.golang.org go get $module@$version
	cd $gopath/pkg/mod/$module@$version/$path
	CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -mod=readonly -o $tmpdir/$name -x -trimpath -ldflags -buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000

Results are stored in ./data/.
Builds are done with a home directory of ./home/.
Toolchains are in ./sdk/.


# Todo

- Should not strip entire buildid. I think it was needed in the past. Might need rules for some older Go versions that do need it.

- Test with repo's with uppercase characters. Goproxy should uppercase-encode them, Azure -> !azure
- See how major version changes work. We will specify eg /v2/ at the end of the go module name.
- See if builds with replaces in go.mod can work.
- Do a test with replacing placeholder 0000 requirements with a replace statement with actual version numbers. Perhaps goproxy will grok that.

- Create a transparency log with builds.
- Have multiple machines (ideally different goos & goarch, also different user, workdir) do a build in parallel, require/test the results are the same.

- Write tests.
- Could handle more versions, like @master, @commitid, etc.
- Cache responses from goproxy?
- Handle special characters in modules/versions/package paths.
- Perhaps understand "/..." package syntax to build all commands in a module or package dir
- Cleanup dir in go/pkg/mod/ after fetching/building, saves disk space. And we won't redownload too often. We could also periodically remove dirs with atime older than 1 hour. Will help if people build one module for different goversion/goos/goarch.
