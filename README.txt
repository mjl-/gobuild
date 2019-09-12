Gobuild serves reproducibly built binaries from sources via go module proxy.

Gobuild runs on http://localhost:8000 by default. Example URL it serves:

	http://localhost:8000/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.13

More generally:

	http://localhost:8000/<module>@<version>/<path>/<goos>-<goarch>-<goversion>{,.log,.sha256}

Gobuild fetches code through https://proxy.golang.org/. Then builds by
executing commands like:

	cd $tmpdir
	GO111MODULE=on GOPROXY=https://proxy.golang.org go get $module@$version
	cd $gopath/pkg/mod/$module@$version/$path
	CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -o <$tmpdir/name> -x -trimpath -ldflags -buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000

Results are stored in ./data/.
The toolchains are expected to be in ./sdk/ as created by eg "go get golang.org/dl/go1.13 && go1.13 download".

Note: should not strip the entire buildid, probably only the first two hash (compiler actions).

Created as demo for lightning talk at an Go Amsterdam Meetup.

Unimplemented idea: create a transparency log for reproducibly built binaries.
