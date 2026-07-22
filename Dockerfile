# Pin base images to SHA digests for reproducible, supply-chain-safe builds.
# Update with: docker buildx imagetools inspect <image> --format '{{.Manifest.Digest}}'
FROM golang:1.26.5-alpine@sha256:99e12cfb19b753915f9b9fdc5a99f1869a24a69d3a0955832d5702e7fa68f1be AS builder

WORKDIR /build

ENV GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*"
ENV GONOSUMDB="github.com/BCBP-SOLUTIONS-FZC-LLC/*"

# hadolint ignore=DL3018
RUN apk add --no-cache git

ARG BUILD_VERSION=dev
ARG SOURCE_DATE_EPOCH=0

COPY go.mod go.sum ./

RUN --mount=type=secret,id=go_private_token \
    TOKEN=$(cat /run/secrets/go_private_token 2>/dev/null || true) && \
    if [ -z "$TOKEN" ]; then echo "ERROR: go_private_token secret is missing or empty — pass --secret id=go_private_token,src=<token-file>"; exit 1; fi && \
    git config --global url."https://x-access-token:${TOKEN}@github.com/".insteadOf "https://github.com/" && \
    go mod download && \
    git config --global --unset url."https://x-access-token:${TOKEN}@github.com/".insteadOf

COPY --link . .

RUN CGO_ENABLED=0 GOOS=linux \
    SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    go build -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o bin/iam-org-membership ./cmd/server && \
    go build -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o bin/reconciler ./cmd/reconciler

FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639

ARG BUILD_VERSION=dev
ENV BUILD_VERSION=${BUILD_VERSION}

LABEL org.opencontainers.image.title="iam-org-membership" \
      org.opencontainers.image.source="https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership" \
      org.opencontainers.image.vendor="BCBP Solutions FZC LLC" \
      org.opencontainers.image.revision="${BUILD_VERSION}"

COPY --from=builder /build/bin/iam-org-membership /iam-org-membership
COPY --from=builder /build/bin/reconciler /reconciler

EXPOSE 8080

ENTRYPOINT ["/iam-org-membership"]
