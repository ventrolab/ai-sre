# Go components of the root module. Build context: repository root.
#   docker build -f build/go.Dockerfile --build-arg CMD=services/tool-hub -t tool-hub .
# The builder runs on the build platform and cross-compiles (buildx multi-arch without QEMU).
FROM --platform=$BUILDPLATFORM golang:1.25.14 AS build
ARG CMD
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY pkg ./pkg
COPY services/tool-hub ./services/tool-hub
COPY agents ./agents
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/app ./${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
