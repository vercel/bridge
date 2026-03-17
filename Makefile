.PHONY: all proto clean

all: clean proto

# Generate all protobuf outputs (Go, TypeScript, JSON Schema)
proto:
	buf generate
	buf generate --template buf.gen.jsonschema.yaml
	go run ./scripts/deref-jsonschema/ api/jsonschema
	rm -rf pkg/embeds/jsonschema && cp -r api/jsonschema pkg/embeds/jsonschema
	cd api/go && go mod tidy
	cd api/ts && npm install

# Clean generated files
clean:
	rm -rf api/go/bridge
	rm -rf api/ts/bridge
	rm -rf api/jsonschema
	rm -rf pkg/embeds/jsonschema
