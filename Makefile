SHELL := /bin/sh

GOLDEN_SEARCH_CORPUS    := tests/golden/search/corpus
GOLDEN_SEARCH_QUESTIONS := tests/golden/search/questions.jsonl

.PHONY: all java go cpp test test-java test-go test-cpp interop eval clean

all: test

java:
	$(MAKE) -C java jar

go:
	cd go && go build -o build/snapvault ./cmd/snapvault

cpp:
	cmake -S cpp -B cpp/build -DCMAKE_BUILD_TYPE=Release
	cmake --build cpp/build

test: test-java test-go test-cpp

test-java:
	$(MAKE) -C java test

test-go:
	cd go && gofmt -l . && test -z "$$(gofmt -l .)" && go vet ./... && go test ./...

test-cpp: cpp
	ctest --test-dir cpp/build --output-on-failure

interop: java go cpp
	tests/interop.sh

# eval runs the golden search-eval question set against the golden corpus
# with both embedders. The builtin run needs nothing installed; the static
# run needs the potion-base-8M model (`go/build/snapvault model pull
# potion-base-8M`) and, if it's missing, fails with the CLI's own clear
# "model potion-base-8M is not installed" message and a non-zero exit,
# stopping this target rather than continuing silently.
eval: go
	./go/build/snapvault eval run --corpus $(GOLDEN_SEARCH_CORPUS) --questions $(GOLDEN_SEARCH_QUESTIONS) --embedder builtin
	./go/build/snapvault eval run --corpus $(GOLDEN_SEARCH_CORPUS) --questions $(GOLDEN_SEARCH_QUESTIONS) --embedder static

clean:
	$(MAKE) -C java clean
	rm -rf go/build cpp/build
