build:
	CGO_ENABLED=0 go build
	CGO_ENABLED=0 go vet

check:
	GOOS=linux GOARCH=386 CGO_ENABLED=0 go vet
	staticcheck

fmt:
	gofmt -w -s *.go

buildall:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm go build
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build
	CGO_ENABLED=0 GOOS=linux GOARCH=386 go build
	CGO_ENABLED=0 GOOS=openbsd GOARCH=amd64 go build
	CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build
	CGO_ENABLED=0 GOOS=netbsd GOARCH=amd64 go build
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build
	CGO_ENABLED=0 GOOS=dragonfly GOARCH=amd64 go build
	CGO_ENABLED=0 GOOS=illumos GOARCH=amd64 go build
	CGO_ENABLED=0 GOOS=solaris GOARCH=amd64 go build
	CGO_ENABLED=0 GOOS=aix GOARCH=ppc64 go build
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build
	CGO_ENABLED=0 GOOS=plan9 GOARCH=amd64 go build
