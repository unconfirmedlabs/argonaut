GO ?= go

GO_RELEASE_FLAGS := -mod=readonly -trimpath -buildvcs=false -tags=netgo,osusergo -ldflags=-s\ -w\ -buildid=

.PHONY: test race vet build reproducible clean

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GO_RELEASE_FLAGS) -o argonaut .

reproducible:
	scripts/repro-build.sh

clean:
	rm -rf argonaut dist
