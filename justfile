next_tag := `tapit nextgitrelease`

# Build prefix_updater binary
build:
    go build -o prefix_updater .

# Build a stripped, CGO-free release binary for the host platform
build-release:
    CGO_ENABLED=0 go build -ldflags "-s -w" -o prefix_updater .

# Build the release binaries for linux/amd64 and linux/arm64
build-all:
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o dist/prefix_updater_linux_amd64 .
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" -o dist/prefix_updater_linux_arm64 .

# Run the binary
run *ARGS:
    go run . {{ARGS}}

# Clean build artifacts
clean:
    rm -rf prefix_updater dist

# Run tests
test:
    go test ./...

# Format code
fmt:
    go fmt ./...

# Vet code
vet:
    go vet ./...

# Tidy modules
tidy:
    go mod tidy

# Everything CI runs
check: fmt vet test

# Tag the next release and push it
tag:
    git tag -f -a "{{next_tag}}" -m "{{next_tag}}" && git push --tags

# Move the latest release tag to HEAD and force push it
retag:
    git tag -f -a `tapit latestgitrelease` -m `tapit latestgitrelease` && git push --tags -f

format:
    goimports -w=true $(find . -type f -name '*.go' -not -path './vendor/*')
