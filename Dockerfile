FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /jomock .
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /jomock /jomock
# /data owned by nonroot (65532) so the stub/proto cache is writable
COPY --from=build --chown=65532:65532 /data /data
WORKDIR /data
EXPOSE 8080 8081
ENTRYPOINT ["/jomock"]
