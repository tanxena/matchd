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

.PHONY: build test test-long test-race-loop lint run bench bench-build bench-smoke bench-full bench-balanced bench-report ci fmt-check clean

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

## test-race-loop: run one package under -race N times, every run's -v
## output appended to a log, to catch a failure that happens rarely.
##   make test-race-loop N=50 PKG=./internal/server/
N   ?= 50
PKG ?= ./internal/server/
RACE_LOG ?= $(BIN)/race-loop.log
test-race-loop:
	@mkdir -p $(BIN); : > $(RACE_LOG); fails=0; 	for i in $$(seq 1 $(N)); do 		echo "=== run $$i of $(N): go test -race -count=1 -v $(PKG)" >> $(RACE_LOG); 		if $(GO) test -race -count=1 -v -timeout 10m $(PKG) >> $(RACE_LOG) 2>&1; then 			echo "run $$i: ok"; 		else 			echo "run $$i: FAIL"; echo "=== run $$i FAILED" >> $(RACE_LOG); fails=$$((fails+1)); 		fi; 	done; 	echo "$$fails of $(N) runs failed; output in $(RACE_LOG)"; [ $$fails -eq 0 ]

## lint: golangci-lint with its default linters
lint:
	golangci-lint run ./...

## run: start an engine on ./data with one instrument
run: build
	$(BIN)/engine$(EXE) -data ./data -instrument XYZ:1:1

## bench: every benchmark, with allocation counts
bench:
	$(GO) test -run '^$$' -bench . -benchmem ./...

## bench-build: the engine and the benchmark programs into bin/
bench-build:
	$(GO) build -o $(BIN)/ ./cmd/engine ./bench/loadgen ./bench/clockprobe ./bench/benchrun ./bench/report

## bench-smoke: every benchmark configuration once, briefly (about 2 min),
## then the report generator over the results: does the harness work?
## Results go to bin/bench-smoke (not committed).
bench-smoke: bench-build
	$(BIN)/benchrun$(EXE) -suite smoke -results $(BIN)/bench-smoke
	$(BIN)/report$(EXE) -results $(BIN)/bench-smoke -smoke -min-runs 1 -out $(BIN)/bench-smoke/report.md
	@echo "report: $(BIN)/bench-smoke/report.md"

## bench-full: the benchmark under the High performance plan (about 75
## min; read bench/CHECKLIST.md first). Results go to bench/results/full.
bench-full: bench-build
	$(BIN)/benchrun$(EXE) -suite full -results bench/results/full

## bench-balanced: the resting and clock rows under the Balanced plan
## (about 10 min). Results go to bench/results/balanced.
bench-balanced: bench-build
	$(BIN)/benchrun$(EXE) -suite balanced -results bench/results/balanced

## bench-report: regenerate the README's benchmark section from bench/results.
## For the committed results: the Balanced resting row has 3 clean runs of
## 5 (2 disturbed by Defender, listed under the tables), accepted as n=3;
## the Defender exclusion was set by the operator and can't be measured.
bench-report: bench-build
	$(BIN)/report$(EXE) -results bench/results -readme README.md \
		-allow-disturbed -min-runs-suite balanced=3 \
		-asserted "Microsoft Defender exclusion on the benchmark data directory (%TEMP%\matchd-bench)"

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
