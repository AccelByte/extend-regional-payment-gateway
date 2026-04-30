.PHONY: proto build test run lint docker-build clean

# Generate protobuf code (requires protoc, protoc-gen-go, protoc-gen-go-grpc,
# protoc-gen-grpc-gateway, protoc-gen-openapiv2 to be installed)
proto:
	@bash proto.sh

# Build the service binary
build: proto
	go build -o payment-bridge .

# Run all tests
test:
	go test ./... -v -count=1

# Run unit tests only (no external deps)
test-unit:
	go test ./internal/... -v -count=1

# Run the service locally — loads .env.local if it exists
run:
	@if [ -f .env.local ]; then \
		set -a && . ./.env.local && set +a && go run .; \
	else \
		go run .; \
	fi

# Lint
lint:
	golangci-lint run ./...

# Build Docker image
docker-build:
	docker build -t extend-regional-payment-gateway:latest .

# Start local MongoDB for integration tests
mongo-up:
	docker run -d --name mongo-test -p 27017:27017 mongo:6

# Stop and remove local MongoDB
mongo-down:
	docker stop mongo-test && docker rm mongo-test

# Tidy dependencies
tidy:
	go mod tidy

clean:
	rm -f payment-bridge
	rm -rf pkg/pb/* gateway/apidocs/*
