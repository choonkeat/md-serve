.PHONY: build link publish publish-dry test unit-test bump

build: build-platforms link

# Put this checkout's md-serve on PATH. `npm link` symlinks bin/md-serve.js into
# the npm prefix; that shim prefers npm-platforms/<plat>-<arch>/bin/md-serve —
# what build-platforms just wrote — over any published package. So `md-serve`
# means "this working tree" after a build. The version banner is printed as
# proof, because npm link's failures are easy to miss otherwise.
#
# Note this does NOT affect `npx @choonkeat/md-serve`, which resolves from the
# npx cache and will keep running the last published release.
link:
	npm config set prefix $(HOME)/.swe-swe 2>/dev/null; npm link 2>/dev/null || true
	@printf 'linked: %s\n  ' "$$(command -v md-serve || echo '(not on PATH)')"
	@md-serve -version 2>/dev/null || echo '(could not run md-serve -version)'

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
