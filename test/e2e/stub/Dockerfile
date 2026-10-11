FROM --platform=$BUILDPLATFORM golang:1.27.1 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
COPY internal/b2/ ./internal/b2/
COPY test/e2e/stub/ ./test/e2e/stub/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o stub ./test/e2e/stub

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/stub /stub
USER 65532:65532
ENTRYPOINT ["/stub"]
