# Git Branching Workflow

This document explains how our team branches, reviews, and merges changes in the `snapvault` repository.
Follow it for any change larger than a one-line fix.

## Branch naming

Branches follow the pattern `<initials>/<short-topic>`, for example `jw/fix-index-panic`.
Branch names longer than 40 characters are rejected by the pre-push hook.

## Starting a branch

Create a branch from the latest `main`:

```bash
git fetch origin
git checkout -b jw/fix-index-panic origin/main
```

Rebase onto `main` before opening a pull request, rather than merging `main` into your branch.
Merge commits inside feature branches make the history hard to bisect and are the single biggest complaint in the last survey of the team.

## Opening a pull request

Every pull request needs a one-sentence summary and a test plan.
Pull requests that touch `go/internal/search/` or `go/internal/model2vec/` require a review from at least two engineers, since those packages feed the retrieval pipeline directly.

## Commit messages

Commit subject lines are capped at 72 characters and use the imperative mood, for example "Add BM25 length normalization" rather than "Added" or "Adding".
A body paragraph explaining why is required for anything that changes default behavior.

## Merging

We use squash merges exclusively; the "Rebase and merge" button is disabled at the organization level.
After a squash merge, delete the remote branch:

```bash
git push origin --delete jw/fix-index-panic
```

## CI gates

A pull request cannot merge until three checks pass: `go vet`, `gofmt -l`, and the full test suite, which currently takes about eleven minutes on the shared runner.
Flaky tests should be quarantined with an issue link, never silently retried in a loop.
