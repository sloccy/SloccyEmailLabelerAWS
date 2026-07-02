# Invoked by `sam build` (each function's Metadata.BuildMethod: makefile in template.yaml).
# SAM calls `make build-<LogicalId>` per function with ARTIFACTS_DIR set to that function's
# staging directory. All three functions share one Go binary; MODE selects behavior at
# runtime (see main.go), so the build step is identical — only the target name differs.
build-WebFunction build-ScanFunction build-PushFunction:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o $(ARTIFACTS_DIR)/bootstrap .
