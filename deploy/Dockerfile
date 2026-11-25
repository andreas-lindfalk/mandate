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
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -o /pgmandate ./cmd/pgmandate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pgmandate /pgmandate
EXPOSE 8585
ENTRYPOINT ["/pgmandate"]
CMD ["serve"]
