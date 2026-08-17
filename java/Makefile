SHELL := /bin/sh

JAVA_RELEASE ?= 21
MAIN_CLASS := io.snapvault.cli.Main
MAIN_SOURCES := $(shell find src/main/java -name '*.java' -type f | sort)
TEST_SOURCES := $(shell find src/test/java -name '*.java' -type f 2>/dev/null | sort)

.PHONY: all compile jar test clean

all: test jar

compile:
	@mkdir -p build/classes
	javac --release $(JAVA_RELEASE) -Xlint:all -Werror -d build/classes $(MAIN_SOURCES)

jar: compile
	@mkdir -p build
	jar --create --file build/snapvault.jar --main-class $(MAIN_CLASS) -C build/classes .

test: compile
	@mkdir -p build/test-classes
	javac --release $(JAVA_RELEASE) -Xlint:all -Werror -cp build/classes -d build/test-classes $(TEST_SOURCES)
	java -ea -cp build/classes:build/test-classes io.snapvault.AllTests

clean:
	rm -rf "$(CURDIR)/build"
