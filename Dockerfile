# One image recipe for every service:  docker build --build-arg SERVICE=gateway .
FROM golang:1.26-alpine AS build
ARG SERVICE
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/service ./cmd/${SERVICE} \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/service /service
# writable state (the hub's identity key, member cache); named volumes inherit this owner
COPY --from=build --chown=65532:65532 /out/data /data
USER nonroot
ENTRYPOINT ["/service"]
