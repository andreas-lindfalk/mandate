# The web app is built first and embedded into the Go binary, so one image
# serves the UI, the API, MCP and the deliveries.
#
# Both build stages run on the machine that builds ($BUILDPLATFORM), whatever
# the image is for, and Go cross-compiles to the target. Nothing is built
# under emulation: a release for arm64 once hung there, in pnpm install. The
# last stage only copies files, so it needs no emulation either.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
# The release the image is built from, as `pgmandate version` reports it:
#   docker build --build-arg VERSION=v0.1.0 ...
# the release's tag, the same string a go build of that tag reports. Without
# it the binary says "dev": .git is not in the build context. It must be a
# label the log keeps as provenance: letters, digits and . _ : / @ + ( ) -.
ARG VERSION=
# What the image is for, set by the builder: linux and amd64 or arm64.
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN if ! printf '%s\n' "$VERSION" | grep -Eq '^[A-Za-z0-9._:/@+()-]{0,100}$'; then echo "VERSION must be a label: letters, digits and . _ : / @ + ( ) -" >&2; exit 1; fi \
    && mkdir -p /data \
    && CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -ldflags "-X 'github.com/andreas-lindfalk/pgmandate/pkg/version.Version=${VERSION}'" -o /pgmandate ./cmd/pgmandate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pgmandate /pgmandate
# A place the server may write, such as a signing key it makes once. A named
# volume mounted here starts with this ownership.
COPY --from=build --chown=65532:65532 /data /data
EXPOSE 8585
ENTRYPOINT ["/pgmandate"]
CMD ["serve"]
