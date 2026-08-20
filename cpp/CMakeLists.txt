cmake_minimum_required(VERSION 3.16)
project(snapvault-fsck CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

find_package(ZLIB REQUIRED)

# libzstd has no reliable CMake config package on either platform we target,
# so it is located by hand. Homebrew keeps it keg-only under its own prefix
# on both Intel and Apple Silicon macOS; Linux distributions install it into
# the standard system paths, which find_path/find_library search by default.
find_path(ZSTD_INCLUDE_DIR
  NAMES zstd.h
  PATHS /opt/homebrew/opt/zstd/include
        /usr/local/opt/zstd/include
        /opt/homebrew/include
        /usr/local/include
)
find_library(ZSTD_LIBRARY
  NAMES zstd
  PATHS /opt/homebrew/opt/zstd/lib
        /usr/local/opt/zstd/lib
        /opt/homebrew/lib
        /usr/local/lib
)
if(NOT ZSTD_INCLUDE_DIR OR NOT ZSTD_LIBRARY)
  message(FATAL_ERROR
    "libzstd not found; install it (e.g. 'brew install zstd' on macOS or "
    "'apt-get install libzstd-dev' on Linux) and re-run cmake")
endif()
add_library(zstd::zstd UNKNOWN IMPORTED)
set_target_properties(zstd::zstd PROPERTIES
  IMPORTED_LOCATION "${ZSTD_LIBRARY}"
  INTERFACE_INCLUDE_DIRECTORIES "${ZSTD_INCLUDE_DIR}"
)

add_library(fsck_core
  src/sha256.cc
  src/bytes.cc
  src/delta.cc
  src/object_store.cc
  src/parse.cc
  src/fsck.cc
)
target_include_directories(fsck_core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR})
target_link_libraries(fsck_core PUBLIC ZLIB::ZLIB zstd::zstd)
target_compile_options(fsck_core PRIVATE -Wall -Wextra -Werror)

add_executable(snapvault-fsck src/main.cc)
target_link_libraries(snapvault-fsck PRIVATE fsck_core)
target_compile_options(snapvault-fsck PRIVATE -Wall -Wextra -Werror)

enable_testing()

add_executable(fsck_unit_tests tests/unit_tests.cc)
target_link_libraries(fsck_unit_tests PRIVATE fsck_core)
target_compile_options(fsck_unit_tests PRIVATE -Wall -Wextra -Werror)
# Points the golden-delta-vector test at the fixtures shared with the Go and
# Java suites (see tests/golden/v2/delta/MANIFEST.md), independent of
# ctest's working directory.
target_compile_definitions(fsck_unit_tests PRIVATE
  SNAPVAULT_GOLDEN_DELTA_DIR="${CMAKE_CURRENT_SOURCE_DIR}/../tests/golden/v2/delta"
)
add_test(NAME unit COMMAND fsck_unit_tests)

add_test(
  NAME integration
  COMMAND bash ${CMAKE_CURRENT_SOURCE_DIR}/tests/integration_test.sh
          $<TARGET_FILE:snapvault-fsck>
)
