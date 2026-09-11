FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/twenty-tickets ./cmd/twenty-tickets

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && \
    addgroup -g 10001 app && adduser -D -u 10001 -G app app && \
    mkdir /data && chown app:app /data
COPY --from=build /out/twenty-tickets /usr/local/bin/twenty-tickets
USER 10001:10001
ENV PORT=8080 DATA_DIR=/data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["twenty-tickets", "healthcheck"]
ENTRYPOINT ["twenty-tickets"]
