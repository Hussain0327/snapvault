# Code Review Checklist

Use this checklist before approving any pull request in this repository.
It is not exhaustive, but skipping these steps has caused real incidents.

## Before you start

Pull the branch locally and run the test suite yourself; do not review from the diff alone.
Reviewing from the diff alone missed the nil-pointer bug that shipped in version 1.4.2 last November.

## Correctness

Check that every new error path is tested, not just the happy path.
Look for integer overflow on any code that parses lengths from untrusted input, since the decoder in `go/internal/search/index.go` has had two such bugs fixed in the past year.

## Style

Confirm the change matches the Google Go style guide: short receiver names, errors wrapped with `%w`, and no unused imports.
Run `gofmt -l` on the changed files if the CI badge has not posted yet.

## Tests

Every bug fix needs a regression test that fails on the old code and passes on the new code.
A pull request that only adds a fix without a failing-first test is sent back for revision, no exceptions.

## Documentation

If the change alters a public function's behavior, its doc comment must be updated in the same pull request.
Reviewers reject PRs that leave stale doc comments more often than any other single issue, according to the December retrospective.

## Sign-off

Two approvals are required for changes to `go/internal/model2vec/` or `go/internal/search/`, and one approval is enough everywhere else.
Leave a comment explaining your reasoning even when you approve; a bare thumbs-up does not count as a review under our process.
