SHELL := /bin/sh

JAVA_RELEASE ?= 21
MAIN_CLASS := io.snapvault.cli.Main
MAIN_SOURCES := $(shell find src/main/java -name '*.java' -type f | sort)
TEST_SOURCES := $(shell find src/test/java -name '*.java' -type f 2>/dev/null | sort)

# Pinned zstd DECODE dependency (io.airlift:aircompressor, pure Java). Verified against
# io.airlift.compress.zstd.ZstdInputStream via javap before adoption; see java/README or the v2
# design notes for the record of that check. The jar's SHA-256 is pinned so `deps` refuses a
# tampered or mismatched download instead of silently trusting whatever Maven Central returned.
ZSTD_VERSION := 2.0.2
ZSTD_JAR_NAME := aircompressor-$(ZSTD_VERSION).jar
ZSTD_URL := https://repo1.maven.org/maven2/io/airlift/aircompressor/$(ZSTD_VERSION)/$(ZSTD_JAR_NAME)
ZSTD_SHA256 := 0c2a96bf86d5fb24ff7f20c92483c61391cb10d440a4bd278c77365a2b165698
LIB_DIR := lib
LIB_JAR := $(LIB_DIR)/$(ZSTD_JAR_NAME)

.PHONY: all compile jar test clean deps

all: test jar

# Verifies $(LIB_JAR) against the pinned SHA256 on every build, not only
# when it is freshly downloaded: deps is .PHONY, so this recipe always runs,
# even when the $(LIB_JAR) prerequisite was already present and its own
# recipe below never ran. That way a jar dropped or left in java/lib/ by any
# other means is still checked before it reaches the classpath.
deps: $(LIB_JAR)
	echo "$(ZSTD_SHA256)  $(LIB_JAR)" | shasum -a 256 -c -

$(LIB_JAR):
	@mkdir -p $(LIB_DIR)
	curl -fsSL -o "$@.tmp" "$(ZSTD_URL)"
	echo "$(ZSTD_SHA256)  $@.tmp" | shasum -a 256 -c -
	mv "$@.tmp" "$@"

compile: deps
	@mkdir -p build/classes
	javac --release $(JAVA_RELEASE) -Xlint:all -Werror -cp $(LIB_JAR) -d build/classes $(MAIN_SOURCES)

jar: compile
	@mkdir -p build
	printf 'Main-Class: %s\nClass-Path: ../%s\n' "$(MAIN_CLASS)" "$(LIB_JAR)" > build/MANIFEST.MF
	jar --create --file build/snapvault.jar --manifest=build/MANIFEST.MF -C build/classes .

test: deps compile
	@mkdir -p build/test-classes
	javac --release $(JAVA_RELEASE) -Xlint:all -Werror -cp build/classes:$(LIB_JAR) -d build/test-classes $(TEST_SOURCES)
	java -ea -cp build/classes:build/test-classes:$(LIB_JAR) io.snapvault.AllTests

clean:
	rm -rf "$(CURDIR)/build"
