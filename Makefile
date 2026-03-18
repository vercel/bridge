.PHONY: all proto mock clean

all: clean proto mock

# Generate all protobuf outputs (Go, TypeScript, JSON Schema)
proto:
	buf generate
	buf generate --template buf.gen.jsonschema.yaml
	go run ./scripts/deref-jsonschema/ api/jsonschema
	rm -rf pkg/embeds/jsonschema && cp -r api/jsonschema pkg/embeds/jsonschema
	cd api/go && go mod tidy
	cd api/ts && npm install

# Generate mocks
mock:
	mockery

# Clean generated files
clean:
	rm -rf api/go/bridge
	rm -rf api/ts/bridge
	rm -rf api/jsonschema
	rm -rf pkg/embeds/jsonschema
	rm -rf pkg/mocks
