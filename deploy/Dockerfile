# Hub only. Run device Agents natively on the target Linux/macOS account.
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download all
COPY . .
# BuildKit supplies the target platform; do not force an amd64 binary on arm64.
ARG TARGETOS
ARG TARGETARCH
# Leave VERSION empty to report the version declared in the source tree.
ARG VERSION=
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
RUN ldflags="-s -w -X main.component=hub -X main.commit=$COMMIT -X main.buildTime=$BUILD_TIME"; \
    if [ -n "$VERSION" ]; then \
      ldflags="$ldflags -X main.version=$VERSION -X github.com/soren-labs/roost/internal/protocol.Version=$VERSION"; \
    fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "$ldflags" \
    -o /out/roost ./cmd/roost
RUN mkdir -p /licenses /state && chmod 0700 /state && \
    go list -m -f '{{if not .Main}}{{.Path}}@{{.Version}} {{.Dir}}{{end}}' all | \
    while read -r module directory; do \
      [ -n "$module" ] || continue; \
      destination="/licenses/$(printf '%s' "$module" | tr / _)"; \
      mkdir -p "$destination"; \
      for file in "$directory"/LICENSE* "$directory"/LICENCE* "$directory"/COPYING* "$directory"/NOTICE* "$directory"/PATENTS*; do \
        if [ -e "$file" ]; then cp -R "$file" "$destination/"; fi; \
      done; \
    done

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/roost /roost
COPY --from=build /src/LICENSE /LICENSE
COPY --from=build /licenses /licenses
COPY --from=build --chown=10001:10001 /state /state
USER 10001:10001
WORKDIR /state
EXPOSE 8787
ENTRYPOINT ["/roost"]
CMD ["hub", "serve", "--config", "/state/hub.json"]
