SHELL := /bin/sh

.PHONY: all java go cpp test test-java test-go test-cpp interop clean

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

clean:
	$(MAKE) -C java clean
	rm -rf go/build cpp/build
