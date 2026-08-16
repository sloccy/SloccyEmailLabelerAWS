# Invoked by `sam build` (each function's Metadata.BuildMethod: makefile in template.yaml).
# SAM calls `make build-<LogicalId>` per function with ARTIFACTS_DIR set to that function's
# staging directory. All four functions share one Go binary; MODE selects behavior at
# runtime (see main.go), so the build step is identical — only the target name differs.
# static/vendor is gitignored; scripts/vendor.sh fetches the versions pinned in
# package.json before the Go build embeds static/. The file target keeps repeat
# builds (SAM runs make once per function) from re-fetching.
#
# -trimpath is load-bearing for cost, not just hygiene: SAM compiles each function in its
# own scratch directory, and without it Go embeds a build ID derived from that path. The
# four binaries then differ in a handful of bytes despite being the same program, so SAM
# hashes four distinct artifacts and uploads four ~9 MiB copies of it. Trimming the paths
# makes the builds byte-identical, collapsing them to a single upload SAM reuses.
build-WebFunction build-ScanFunction build-PushFunction build-ImproveFunction: static/vendor/htmx.min.js
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(ARTIFACTS_DIR)/bootstrap .

static/vendor/htmx.min.js: package.json scripts/vendor.sh
	./scripts/vendor.sh
