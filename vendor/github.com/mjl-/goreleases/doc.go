// Package goreleases lists all or supported Go toolchain releases, and download/verify/extract them.
//
// A list of releases is retrieved from golang.org/dl/?mode=json, optionally with the include=all parameter.
// The released files are assumed to contain just a directory named "go" with a release.
package goreleases
