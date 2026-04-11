.PHONY: build publish publish-dry test unit-test bump

build: build-platforms
	npm config set prefix $(HOME)/.swe-swe 2>/dev/null; npm link 2>/dev/null || true

test: unit-test

unit-test:
	go vet ./...
	go test ./...

build-platforms:
	./scripts/build-platforms.sh

publish-dry: build-platforms
	DRY_RUN=true ./scripts/publish.sh

publish: build-platforms
	DRY_RUN=false ./scripts/publish.sh

bump:
	@if [ -z "$(VERSION)" ]; then \
		echo "Usage: make bump VERSION=x.y.z"; \
		exit 1; \
	fi
	@echo "Bumping version to $(VERSION)..."
	@node -e 'var fs=require("fs"),p=JSON.parse(fs.readFileSync("package.json","utf8"));p.version="$(VERSION)";for(var k of Object.keys(p.optionalDependencies||{}))p.optionalDependencies[k]="$(VERSION)";fs.writeFileSync("package.json",JSON.stringify(p,null,2)+"\n")'
	@echo "Version bumped to $(VERSION)"
