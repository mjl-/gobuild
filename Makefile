build:
	CGO_ENABLED=0 go build
	CGO_ENABLED=0 go vet

check:
	GOOS=linux GOARCH=386 CGO_ENABLED=0 go vet
	staticcheck

fmt:
	gofmt -w -s *.go
