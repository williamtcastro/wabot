# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/wame . \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wame /usr/local/bin/wame
COPY --from=build --chown=nonroot:nonroot --chmod=700 /out/data /data
ENV WAME_DB=/data/wame.db \
    WAME_ADDR=0.0.0.0:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["wame"]
CMD ["serve"]
