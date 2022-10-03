/*
Gobuild deterministically compiles programs written in Go that are
availablethrough the Go module proxy, and returns the binary.

The Go module proxy at https://proxy.golang.org ensures source code stays
available, and you are highly likely to get the same code each time you fetch
it. Gobuild aims to do the same for binaries.

# URLs

You can compose URLs to a specific module, build or result:

	/<module>
	/<module>@<version>/<package>/<goos>-<goarch>-<goversion>/
	/<module>@<version>/<package>/<goos>-<goarch>-<goversion>/<sum>/

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

# Transparency log

Gobuild maintains a transparency log containing the hashes of all successful
builds, similar to the Go module checksum database at https://sum.golang.org/.
Gobuild's "get" subcommand looks up a content hash through the transparency log,
locally keeping track of the last known tree state.  This ensures the list of
successful builds and their hashes is append-only, and modifications or removals
by the server will be detected when you run "gobuild get".

Examples:

	gobuild get github.com/mjl-/gobuild@latest
	gobuild get -sum 0N7e6zxGtHCObqNBDA_mXKv7-A9M -target linux/amd64 -goversion go1.14.1 github.com/mjl-/gobuild@v0.0.8

# Details

Only "go build" is run, for pure Go code. None of "go test", "go generate",
build tags, cgo, custom compile/link flags, makefiles, etc. This means gobuild
cannot build all Go applications.

Gobuild looks up module versions through the Go module proxy. That's why
shorthandversions like "@v1" don't resolve.

Gobuild automatically downloads a Go toolchain (SDK) from https://go.dev/dl/
when it is first referenced. It also periodically queries that page for the latest
supported releases, for redirecting to the latest supported toolchains.

Gobuild can be configured to verify builds with other gobuild instances,
requiring all to return the same hash for a build to be considered successful.

It's easy to run a local instance, or an instance internal to your organization.

To build, gobuild executes:

	GO19CONCURRENTCOMPILATION=0 GO111MODULE=on GOPROXY=https://proxy.golang.org/ \
		CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
		$goversion install -x -v -trimpath -ldflags=-buildid= -- $module/$package@$version

# Why gobuild

Get binaries for any module without having a Go toolchain installed: Useful when
working on a machine that's not yours, or for your colleagues or friends who
don't have a Go compiler installed.

Simplify your software release process: You no longer need to cross compile for
many architectures and upload binaries to a release page. You never forget a
GOOS/GOARCH target. Just link to the build URL for your module and binaries will
be created on demand.

Binaries for the most recent Go toolchain: Go binaries include the runtime and
standard library of the Go toolchain used to compile, including bugs. Gobuild
links or can redirect to binaries built with the latest Go toolchain, so no need
to publish new binaries after an updated Go toolchain is released.

Verify reproducibility: Configure gobuild to check against other gobuild
instances with different configuration to build trust that your binaries are
indeed reproducible.

# Caveats

A central service like gobuilds.org that provides binaries is an attractive
target for attackers. By only building code available through the Go module
proxy, and only building with official Go toolchains, the options for attack are
limited. Further security measures are the isolation of the gobuild proces and
of the build commands (minimal file system view, mostly read-only; limited
network; disallowing escalation of privileges).

The transparency log is only used when downloading binaries using the "gobuild
get" command, which uses and updates the users local cache of the signed
append-only transparency log with hashes of built binaries. If users only
download binaries through the convenient web interface, no verification of the
transparency log takes place. The transparency log gives the option of
verification, that alone may give users confidence the binaries are not tampered
with. A nice way of continuously verifying that a gobuild instance, such as
gobuilds.org, is behaving correctly is to set up your own gobuild instance that
uses gobuilds.org as URL to verify builds against.

Gobuild will build binaries with different (typically newer) Go toolchains than
an author has tested their software with. So those binary are essentially
untested. This may cause bugs. However, point releases typically contains only
stability/security fixes that don't normally cause issues and are desired. The
Go 1 compatibility promise means code will typically work as intended with new
Go toolchain versions. But an author can always link to a build with a specific
Go toolchain version. A user simply has the additional option to download a
build by a newer Go toolchain version.

# Running

Gobuild should work on all platforms for which you can download a Go toolchain
on https://go.dev/dl/, including Linux, macOS, BSDs, Windows.

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

Keep security in mind when offering public access to your gobuild instance.
Run gobuild in a locked down environment, with restricted system access (files,
network, processes, kernel features), possibly through systemd or with
containers.

You could make all outgoing network traffic go through an HTTPS proxy by
setting an environment variable HTTPS_PROXY=... (refuse all other outgoing
connections). The proxy should allow the following addresses:

	# For fetching Go module source code.
	proxy.golang.org:443
	storage.googleapis.com:443

	# For checking the transparency log of the Go module proxy.
	sum.golang.org:443

	# For listing and fetching go toolchains.
	go.dev:443
	dl.google.com:443

	# Optional, when using ACME with Let's Encrypt for HTTPS.
	acme-v02.api.letsencrypt.org:443

	# Optional, any verifier URLs you have configured.
*/
package main
