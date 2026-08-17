cmake_minimum_required(VERSION 3.16)
project(snapvault-fsck CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

find_package(ZLIB REQUIRED)

add_library(fsck_core
  src/sha256.cc
  src/bytes.cc
  src/object_store.cc
  src/parse.cc
  src/fsck.cc
)
target_include_directories(fsck_core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR})
target_link_libraries(fsck_core PUBLIC ZLIB::ZLIB)
target_compile_options(fsck_core PRIVATE -Wall -Wextra -Werror)

add_executable(snapvault-fsck src/main.cc)
target_link_libraries(snapvault-fsck PRIVATE fsck_core)
target_compile_options(snapvault-fsck PRIVATE -Wall -Wextra -Werror)

enable_testing()

add_executable(fsck_unit_tests tests/unit_tests.cc)
target_link_libraries(fsck_unit_tests PRIVATE fsck_core)
target_compile_options(fsck_unit_tests PRIVATE -Wall -Wextra -Werror)
add_test(NAME unit COMMAND fsck_unit_tests)

add_test(
  NAME integration
  COMMAND bash ${CMAKE_CURRENT_SOURCE_DIR}/tests/integration_test.sh
          $<TARGET_FILE:snapvault-fsck>
)
