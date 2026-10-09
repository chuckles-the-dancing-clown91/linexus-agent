# Linexus RMM agent. Stdlib only; static binaries (CGO_ENABLED=0).
#
#   make            vet + test + build ./rmm-agent for this host
#   make dist       dist/rmm-agent-linux-amd64 and dist/rmm-agent-linux-arm64
#                   (+ SHA256SUMS) — what Nexus serves from LINEXUS_AGENT_BINARY_DIR
#   make clean

GO      ?= go
DIST    ?= dist
LDFLAGS ?= -s -w
ARCHES  ?= amd64 arm64

.PHONY: all build vet test dist clean

all: vet test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o rmm-agent .

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

dist:
	@mkdir -p $(DIST)
	@for arch in $(ARCHES); do \
		echo "building $(DIST)/rmm-agent-linux-$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags="$(LDFLAGS)" \
			-o $(DIST)/rmm-agent-linux-$$arch . || exit 1; \
	done
	@cd $(DIST) && sha256sum rmm-agent-linux-* > SHA256SUMS && cat SHA256SUMS

clean:
	rm -rf $(DIST) rmm-agent
