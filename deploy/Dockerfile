# The web app is built first and embedded into the Go binary, so one image
# serves the UI, the API, MCP and the deliveries.
FROM node:22-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.27 AS build
# The release the image is built from, as `pgmandate version` reports it:
#   docker build --build-arg VERSION=v0.1.0 ...
# the release's tag, the same string a go build of that tag reports. Without
# it the binary says "dev": .git is not in the build context. It must be a
# label the log keeps as provenance: letters, digits and . _ : / @ + ( ) -.
ARG VERSION=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN if ! printf '%s\n' "$VERSION" | grep -Eq '^[A-Za-z0-9._:/@+()-]{0,100}$'; then echo "VERSION must be a label: letters, digits and . _ : / @ + ( ) -" >&2; exit 1; fi \
    && CGO_ENABLED=0 go build -ldflags "-X 'github.com/andreas-lindfalk/pgmandate/pkg/version.Version=${VERSION}'" -o /pgmandate ./cmd/pgmandate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pgmandate /pgmandate
EXPOSE 8585
ENTRYPOINT ["/pgmandate"]
CMD ["serve"]
