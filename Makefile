BINARY  := muxboard
VERSION := 0.1.0
LDFLAGS := -s -w

.PHONY: build run test vet fmt cross clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

run: build
	./$(BINARY) serve

vet:
	go vet ./...
	gofmt -l . | (! grep .)

test:
	go test ./...

# Static binaries for the targets that matter: Android/proot phones are
# arm64, most VPSes are amd64.
cross:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-armv7 .

clean:
	rm -rf $(BINARY) dist
