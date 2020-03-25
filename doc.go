/*
Gobuild deterministically compiles programs written in Go that are available
through the Go module proxy, and returns the binary.

The Go module proxy ensures source code stays available, and you are highly
likely to get the same code each time you fetch it. Gobuild aims to do the same
for binaries.

URLs

	/m/<module>/
	/b/<module>@<version>/<package>/<goos>-<goarch>-<goversion>/
	/r/<module>@<version>/<package>/<goos>-<goarch>-<goversion>/<sum>/

The first URL fetches the requested Go module, and redirects to a URL of the
second form.

The second URL first resolves "latest" for the module and Go version with a
redirect. Then starts a build for the requested parameters if needed. When
finished, it redirects to a URL of the third kind.

The third URL represents a successful build. The URL includes the sum: The
versioned raw-base64-url-encoded 20-byte sha256-prefix. The page links to the
binary, the build output log file, and to builds of the same package with
different module versions, goversions, goos/goarch.

You need not and cannot refresh a build: they would give the same result.

More

Only "go build" is run. None of "go test", "go generate", build tags, cgo,
custom compile/link flags, makefiles, etc.

Gobuild looks up module versions through the go proxy. That's why
shorthandversions like "@v1" don't resolve.

To build, gobuild executes:

	tmpdir=$(mktemp -d)
	cd $tmpdir
	GO111MODULE=on GOPROXY=https://proxy.golang.org/ $goversion get -d -v $module@$version
	cd $HOME/go/pkg/mod/$module@$version/$path
	GO19CONCURRENTCOMPILATION=0 GO111MODULE=on GOPROXY=https://proxy.golang.org/ \
		CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
		$goversion build -mod=readonly -o $tmpdir/$name -x -v -trimpath -ldflags=-buildid=

Running

Start gobuild by running:

	gobuild serve

You can optionally pass a configuration file. Create an example config file
with:

	gobuild config >gobuild.conf


Test it with:

	gobuild testconfig gobuild.conf

By default, build results are stored in ./data, $HOME is set to ./home during
builds and Go toolchains are installed in ./sdk.
*/
package main
