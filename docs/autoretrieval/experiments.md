# Autoretrieval experiments

Every run of the loop in `program.md` appends one row here, whether the
change was kept or discarded.
Nothing in this file is ever edited or removed after the fact; a bad idea
that didn't pan out is exactly as useful a record as one that did.

`FBeta@10` and `Recall@10` are `--embedder static`'s numbers from
`snapvault eval run --questions tests/golden/search/questions.jsonl
--corpus tests/golden/search/corpus`, per `program.md` step 3.

| Date | Change | FBeta@10 before | FBeta@10 after | Recall@10 before (lexical / semantic / hard) | Recall@10 after (lexical / semantic / hard) | Outcome |
| --- | --- | --- | --- | --- | --- | --- |
| 2026-08-25 | baseline (DefaultPipeline: chunk 1200 / overlap 200 / stopwords on; BM25 k1 1.2, b 0.75; RRF k 60) | — | 0.269 | — | 1.000 / 0.975 / 1.000 | baseline recorded in `baseline.json` (builtin: 0.259; static: 0.269) |
