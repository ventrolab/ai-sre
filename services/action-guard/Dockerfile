# Build context: services/action-guard.
# The builder runs on the build platform and cross-compiles (buildx multi-arch without QEMU).
FROM --platform=$BUILDPLATFORM golang:1.23.4 AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY guard ./guard
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/action-guard .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/action-guard /action-guard
USER 65532:65532
ENTRYPOINT ["/action-guard"]
