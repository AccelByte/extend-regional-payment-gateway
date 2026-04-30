# Stage 1: Generate protobuf code
FROM --platform=$BUILDPLATFORM rvolosatovs/protoc:4.1.0 AS proto-builder
WORKDIR /app
COPY pkg/proto ./pkg/proto
COPY proto.sh .
RUN sed -i 's/\r//' proto.sh && bash proto.sh

# Stage 2: Build Go binary
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=proto-builder /app/pkg/pb ./pkg/pb
COPY --from=proto-builder /app/gateway/apidocs ./gateway/apidocs
RUN CGO_ENABLED=0 GOOS=linux go build -tags proto -ldflags="-w -s" -o /payment-bridge .

# Stage 3: Minimal runtime image
FROM gcr.io/distroless/static-debian12
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /payment-bridge /payment-bridge
COPY --from=builder /app/gateway /gateway
COPY --from=builder /app/third_party /third_party
EXPOSE 6565 8000 8080
ENTRYPOINT ["/payment-bridge"]
