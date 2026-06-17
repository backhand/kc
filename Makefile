# kc — build targets
#
# Requires Go >= 1.25 (the go directive in go.mod; raised by golang.org/x/sys).
# The shipped binary depends only on the Charm libs (bubbletea/lipgloss/bubbles)
# and the Go runtime — no cgo, no system libraries beyond the OS baseline.

BIN        := kc
PKG        := .
LDFLAGS_SW := -s -w

.PHONY: build build-small test vet fmt clean linux linux-small dist

# Native static build (no cgo).
build:
	CGO_ENABLED=0 go build -o $(BIN) $(PKG)

# Native static build, trimmed (strip symbol table + DWARF).
build-small:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS_SW)" -o $(BIN) $(PKG)

# Linux/amd64 static cross-compile — ONE command, no native packages fetched.
linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o $(BIN)-linux-amd64 $(PKG)

# Linux/amd64 static cross-compile, trimmed.
linux-small:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS_SW)" -o $(BIN)-linux-amd64 $(PKG)

# Build trimmed binaries for the platforms we ship to.
dist: build-small linux-small

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -f $(BIN) $(BIN)-linux-amd64
