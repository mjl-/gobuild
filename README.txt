Gobuild serves reproducibly built binaries from sources via go module proxy.

Gobuild serves on http://localhost:8000 by default. Example:

	http://localhost:8000/x/linux-amd64-go1.13/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/

URLs are of the form:

	http://localhost:8000/x/<goos>-<goarch>-<goversion>/<module>@<version>/<path>/{,log,sha256,dl,<sum>}

Gobuild builds by executing commands like:

	cd $tmpdir
	GO111MODULE=on GOPROXY=https://proxy.golang.org go get $module@$version
	cd $gopath/pkg/mod/$module@$version/$path
	CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -o <$tmpdir/name> -x -trimpath -ldflags -buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000

Results are stored in ./data/.
Builds are done with a home directory of ./home/.
Toolchains are in ./sdk/.

Created as demo for lightning talk at an Go Amsterdam Meetup.

# Todo

- Add timestamp to data/builds.txt, might come in handy.
- Keep track of all builds, and show in UI when linking to a build that it has already finished. You don't want to click around and have to wait long time before getting a page.
- Gzip binaries and log files, and return the compressed version if the user-agent understands it. Should save disk space, bandwidth and compression at runtime.
- Build in temp dir, move to final name when done. Do in-process coordination between goroutines so we don't do the same build for different requests.
- For some outputs, return a different filename. Eg with .exe for windows. Probably more.
- Keep a queue of builds that are being requested. If we don't have a module/version/package build ready, we return an HTML page saying the build will be created. The page connects with SSE to register the desire for the build, and to stay up to date on the build schedule. If we leave the page, your position in the queue is dropped. We can perhaps do runtime.NumCPU()+1 builds at the same time.
- parse the html templates once at startup, not for every request.

- Should not strip entire buildid. I think it was needed in the past. Might need rules for some older Go versions that do need it.
- Create a transparency log with builds.

- Write tests...
- Should store failed builds (other than temp failures). So we don't try them over and over again.
- On main page, perhaps a list of recent additions to the Go modules mirror (would have to know which are buildable as binaries).
- Check if versions like "latest" can be requested from module mirror, and whether we should resolve them with a redirect.
- Handle special characters in modules/versions/package paths...
