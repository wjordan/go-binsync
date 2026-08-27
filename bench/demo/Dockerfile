# The patch explorer (docs/DEMO.md). Build from the repository root:
#   bench/demo/build-assets.sh                       # writes bench/out/demo-assets
#   docker build -f bench/demo/Dockerfile -t binsync-demo .
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /demo ./bench/demo

# The server reads only its assets and answers only HTTP, so it needs nothing
# else: no shell, no package manager, no zstd or hdiffz -- the comparison
# objects were built once, by build-assets.sh, and are served as bytes.
FROM scratch
COPY --from=build /demo /demo
COPY bench/out/demo-assets /assets
ENV ASSETS=/assets ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/demo"]
