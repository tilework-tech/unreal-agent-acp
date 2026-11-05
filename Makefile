ACP_SCHEMA_V1 := 1.24.1
ACP_SCHEMA_V2 := 2.0.0-alpha.7
SCHEMA_URL := https://github.com/agentclientprotocol/agent-client-protocol/releases/download

.PHONY: build test vet generate schemas tck vendor-diff snapshot

build:
	go build -o bin/unreal-agent-acp ./cmd/unreal-agent-acp

test:
	go test -race ./...
	cd tools/acpgen && go test ./...

vet:
	go vet ./...
	test -z "$$(gofmt -l . | grep -v '^.cache/')"

# Regenerates acp/v1 and acp/v2 from the pinned schemas.
generate:
	cd tools/acpgen && go run . -schema ../../schema/v1 -out ../../acp/v1 -package acp
	cd tools/acpgen && go run . -schema ../../schema/v2 -out ../../acp/v2 -package acp
	gofmt -w acp

# Re-downloads the pinned schemas; bump the versions above first.
schemas:
	mkdir -p schema/v1 schema/v2
	for f in schema.json meta.json; do \
		curl -fsSL -o schema/v1/$$f $(SCHEMA_URL)/schema-v$(ACP_SCHEMA_V1)/$$f; \
		curl -fsSL -o schema/v2/$$f $(SCHEMA_URL)/schema-v$(ACP_SCHEMA_V2)/$$f; \
	done
	echo $(ACP_SCHEMA_V1) > schema/v1/VERSION
	echo $(ACP_SCHEMA_V2) > schema/v2/VERSION

# Runs the ACP conformance suite against the built binary and a fake model.
tck: build
	./scripts/tck.sh

vendor-diff:
	./scripts/vendor-diff.sh $(TO)

# Builds the release archives into dist/ without publishing (needs goreleaser v2).
snapshot:
	goreleaser check
	goreleaser release --snapshot --clean
