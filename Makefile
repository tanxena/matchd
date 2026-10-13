# matchd: build, test, check (PRD 6). Needs GNU make. On Windows, MSYS2's
# make works (C:\msys64\usr\bin\make.exe, with gcc on PATH for -race) when
# run from PowerShell, cmd or an MSYS2 shell. Not from Git Bash: its MSYS
# runtime differs from MSYS2's, and make then starts with an empty
# environment (no TMP, so Go can't create its work directory).

GO   ?= go
BIN  := bin
EXE  := $(if $(filter Windows_NT,$(OS)),.exe,)

# The race detector needs cgo.
export CGO_ENABLED := 1

.PHONY: build test test-long lint run bench ci fmt-check clean

## build: engine and engctl into bin/
build:
	$(GO) build -o $(BIN)/ ./cmd/...

## test: the normal suite, with the race detector (includes 5 crash runs)
test:
	$(GO) test -race -count=1 ./...

## test-long: the acceptance-sized runs: the 1M-command differential test
## over 20 seeds (acceptance 1) and 50 kill-and-recover runs (acceptance 2).
## The package comes before -long and -crash: go test hands everything after
## an unknown flag to the test binary, package path included.
test-long:
	$(GO) test -count=1 -timeout 60m ./internal/book/ -run TestDifferential -long
	$(GO) test -count=1 -timeout 60m ./internal/crashtest/ -run TestCrashRecovery -crash

## lint: golangci-lint with its default linters
lint:
	golangci-lint run ./...

## run: start an engine on ./data with one instrument
run: build
	$(BIN)/engine$(EXE) -data ./data -instrument XYZ:1:1

## bench: every benchmark, with allocation counts
bench:
	$(GO) test -run '^$$' -bench . -benchmem ./...

## ci: what CI runs, plus a Linux cross-compile of every binary and test
## binary. Linux is compiled here but not run; the GitHub Actions workflow
## runs the suite on Linux once the repository is pushed.
ci: fmt-check
	$(GO) vet ./...
	GOOS=linux CGO_ENABLED=0 $(GO) vet ./...
	golangci-lint run ./...
	GOOS=linux CGO_ENABLED=0 $(GO) build -o $(BIN)/linux/ ./cmd/...
	@mkdir -p $(BIN)/linux-tests
	@for p in $$(GOOS=linux $(GO) list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...); do \
		echo "GOOS=linux go test -c $$p"; \
		GOOS=linux CGO_ENABLED=0 $(GO) test -c -o $(BIN)/linux-tests/$$(echo $$p | tr / _).test $$p || exit 1; \
	done
	$(GO) test -race -count=1 ./...

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

clean:
	rm -rf $(BIN)
