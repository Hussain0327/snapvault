# Sprint 14 Standup Notes - March 3, 2026

Attendees: Marcus Ito, Dana Whitfield, Priya Nair, Sam Okafor.

## Decisions

The team decided to postpone the search index migration to SVX2 until Sprint 15, because the model2vec loader is not yet merged.
Marcus is the owner of the migration and will send an updated timeline by March 6.

We agreed to cap the eval corpus at 100 questions for the first release rather than the 250 originally proposed, to keep `make eval` under two minutes in CI.
Priya will trim the question set by Friday.

## Blockers

Sam reported that the `snapvault model pull` command times out on the office network when downloading `model.safetensors`, which is about 30 MB.
Dana is investigating whether the proxy is throttling the connection and will report back at Thursday's standup.

## Action items

- Marcus: draft the SVX2 rollout plan, due March 6.
- Priya: cut the question set to 100 entries, due March 6.
- Dana: root-cause the model pull timeout, due March 5.
- Sam: write a regression test for the timeout once the cause is known.

## Notes

Dana raised a concern that the BM25 implementation does not currently handle ties consistently across runs.
The team agreed this is a bug, not a tuning question, and Marcus filed it as issue #214 with priority P2.

Next standup is Thursday, March 5, at 10:00 in the usual video call.
