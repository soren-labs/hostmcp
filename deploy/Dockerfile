# Hub only. Run device Agents natively on the target Linux/macOS account.
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download all
COPY . .
# BuildKit supplies the target platform; do not force an amd64 binary on arm64.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.2.1
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildTime=$BUILD_TIME" \
    -o /out/hostmcp ./cmd/hostmcp
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
COPY --from=build /out/hostmcp /hostmcp
COPY --from=build /src/LICENSE /LICENSE
COPY --from=build /src/NOTICE /NOTICE
COPY --from=build /licenses /licenses
COPY --from=build /src/reference/chat-on-steroids/docs/licenses/codex /licenses/upstream-codex
COPY --from=build --chown=10001:10001 /state /state
USER 10001:10001
WORKDIR /state
EXPOSE 8787
ENTRYPOINT ["/hostmcp"]
CMD ["hub", "serve", "--config", "/state/hub.json"]
