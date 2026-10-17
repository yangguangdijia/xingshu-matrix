VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.Version=$(VERSION)

.PHONY: build test vet run docker package
build:
	cd server && CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o ../bin/matrix-server ./cmd/matrix-server
	cd server && CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o ../bin/matrix-agent ./cmd/matrix-agent || true
test:
	cd server && go test ./...
vet:
	cd server && go vet ./...
run:
	cd server && MATRIX_MODE=dev MATRIX_STATIC_DIR=../public go run ./cmd/matrix-server
docker:
	docker build --build-arg VERSION=$(VERSION) -t xingshu/matrix-server:$(VERSION) .
package:
	bash scripts/package.sh
