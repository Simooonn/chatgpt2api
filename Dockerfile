FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG APP_VERSION=""

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN LDFLAGS="-s -w"; \
    if [ -n "${APP_VERSION}" ]; then \
      LDFLAGS="${LDFLAGS} -X github.com/basketikun/chatgpt2api/internal/config.BuildVersion=${APP_VERSION}"; \
    fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="${LDFLAGS}" -o /chatgpt2api .


FROM alpine:3.22 AS app

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=build /chatgpt2api ./
COPY web ./web
COPY VERSION ./
COPY config.example.yaml ./config.yaml

ENV PORT=80
EXPOSE 80

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${PORT:-80}/version" >/dev/null || exit 1

CMD ["./chatgpt2api"]
