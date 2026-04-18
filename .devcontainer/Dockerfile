FROM mcr.microsoft.com/devcontainers/go:1-1.25-bookworm

ARG GOLANGCI_LINT_VERSION=v2.11.4

RUN apt-get update \
	&& export DEBIAN_FRONTEND=noninteractive \
	&& apt-get install -y --no-install-recommends \
		e2fsprogs \
		kmod \
		util-linux \
	&& rm -rf /var/lib/apt/lists/* \
	&& curl -sSfL "https://raw.githubusercontent.com/golangci/golangci-lint/${GOLANGCI_LINT_VERSION}/install.sh" \
		| sh -s -- -b /usr/local/bin "${GOLANGCI_LINT_VERSION}"

RUN go env -w GOFLAGS=-tags=integration
