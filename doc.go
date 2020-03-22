// Gobuild deterministically compiles Go code available through the Go module proxy, and returns the binary.//
//
// The Go module proxy ensures source code stays available, and you are likely to
// get the same code each time you fetch it. Gobuild aims to achieve the same for
// binaries.
//
// URLs
//
//	/m/<module>/
//	/x/<goos>-<goarch>-<goversion>/<module>/@<version>/<package>/
//	/z/<sum>/<goos>-<goarch>-<goversion>/<module>/@<version>/<package>/
//
// The first URL fetches the requested Go module, and redirects to a URL of the
// second form.
//
// The second URL starts a build for the requested parameters. When finished, it
// redirects to a URL of the third form.
//
// The third URL is for a successful build. The URL includes the hash, the
// raw-base64-url-encoded 20-byte sha256-prefix. The page has links to download the
// binary, get the build output log file, and cross references to builds of the
// same package with different module versions, goversion, goos, goarch.
//
// You need not and cannot refresh a build, because they are reproducible.
//
// More
//
// Only "go build" is run. No of "go test", "go generate", build tags, cgo, custom
// compile/link flags, makefiles, etc.
//
// Gobuild looks up modules through the go proxy. That's why shorthand versions
// like "@v1" don't resolve.
//
// To build, gobuild executes:
//
// 	tmpdir=$(mktemp -d)
// 	GO111MODULE=on GOPROXY=https://proxy.golang.org/ go get -d -v $module@$version
// 	cd $HOME/go/pkg/mod/$module@$version/$path
// 	GO111MODULE=on GOPROXY=https://proxy.golang.org/ CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
//		go build -mod=readonly -o $tmpdir/$name -x -v -trimpath \
//		-ldflags -buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000
//
// Running
//
// Start gobuild by running:
//
//	gobuild serve
//
// You can optionally pass a configuration file. Create an example config file
// with:
//
//	gobuild config >gobuild.conf
//
//
// Test it with:
//
//	gobuild testconfig gobuild.conf
//
// By default, build results are stored in ./data, $HOME is set to ./home during
// builds and Go toolchains are installed in ./sdk.
package main
