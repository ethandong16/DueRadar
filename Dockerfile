FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} go build -trimpath -ldflags="-s -w" -o /out/dueradar ./cmd/dueradar
RUN mkdir -p /empty

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/dueradar /dueradar
COPY --from=build --chown=65532:65532 /empty /data
USER 65532:65532
ENV DUERADAR_LISTEN=0.0.0.0:8080 DUERADAR_DATA_DIR=/data
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=6s CMD ["/dueradar", "-healthcheck", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["/dueradar"]
