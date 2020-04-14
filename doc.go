/*
Gobuild deterministically compiles programs written in Go that are available
through the Go module proxy, and returns the binary.

The Go module proxy ensures source code stays available, and you are highly
likely to get the same code each time you fetch it. Gobuild aims to do the same
for binaries.


URLs

	/m/<module>
	/b/<module>@<version>/<package>/<goos>-<goarch>-<goversion>/
	/r/<module>@<version>/<package>/<goos>-<goarch>-<goversion>/<sum>/

The first URL fetches the requested Go module to find the commands (main
packages). In case of a single command, it redirects to a URL of the second
form. In case of multiple commands, it lists them, linking to URLs of the second
form. Links are to the latest module and Go versions, and with goos/goarch
guessed based on user-agent.

The second URL first resolves "latest" for the module and Go version with a
redirect. For URLs with explicit versions, it starts a build for the requested
parameters if no build is available yet. After a successful build, it redirects
to a URL of the third kind.

The third URL represents a successful build. The URL includes the sum: The
versioned raw-base64-url-encoded 20-byte prefix of the sha256 sum. The page
links to the binary, the build output log file, and to builds of the same
command with different module versions, goversions, goos/goarch.

You need not and cannot refresh a successful build: they would give the same result.


Transparency log

Gobuild maintains a transparency log containing the hashes of all successful
builds, similar to the Go module checksum database. Gobuild's "get" subcommand
looks up a content hash through the transparency log, locally keeping track of
the latest known hash.  This ensures the list of successful builds and their
hashes is append-only, and modifications or removals by the server will be
detected when you run "gobuild get".

Examples:

	gobuild get github.com/mjl-/gobuild@latest
	gobuild get -sum 0N7e6zxGtHCObqNBDA_mXKv7-A9M -target linux/amd64 -goversion go1.14.1 github.com/mjl-/gobuild@v0.0.8


More

Only "go build" is run, for pure Go code. None of "go test", "go generate",
build tags, cgo, custom compile/link flags, makefiles, etc. This means gobuild
cannot build all Go applications.

Gobuild looks up module versions through the Go module proxy. That's why
shorthandversions like "@v1" don't resolve.

Gobuild automatically downloads a Go toolchain (SDK) from https://golang.org/dl/
when it is first referenced. It also periodically queries that page for the latest
supported releases, for redirecting to the latest supported toolchains.

Gobuild can be configured to verify builds with other gobuild instances,
requiring all to return the same hash for a build to be considered successful.

It's easy to run a local or internal gobuild instance.

To build, gobuild executes:

	GO19CONCURRENTCOMPILATION=0 GO111MODULE=on GOPROXY=https://proxy.golang.org/ \
		CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
		$goversion get -x -v -trimpath -ldflags=-buildid= -- $module/$package@$version


Running

Gobuild should work on all platforms for which you can download a Go toolchain
on https://golang.org/dl/, including LInux, macOS, BSDs, Windows.

Start gobuild by running:

	gobuild serve

You can optionally pass a configuration file. Create an example config file
with:

	gobuild config >gobuild.conf


Test it with:

	gobuild testconfig gobuild.conf

By default, build results and sumdb files are stored in ./data, $HOME is set to
./home during builds and Go toolchains are installed in ./sdk.

You can configure your own signer key for your transparency log. Create new keys with:

	gobuild genkey you.example.org

Now configure the signer key in the config file. And run "gobuild get" with the
-verifierkey flag.
*/
package main
