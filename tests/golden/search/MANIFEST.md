# Search golden set

This is the fixture behind `go/internal/eval`'s retrieval-quality harness: a
corpus of original documents plus a question set, both frozen enough to run
in CI as a quality floor for `snapvault find`.
`go/internal/eval/golden_test.go` reads both, runs the ranking pipeline
against the corpus with each embedder, scores the results against
`questions.jsonl`, and compares the headline `FBeta@10` against
`baseline.json`.

## Layout

- `corpus/<domain>/<slug>.md`: about 50 original documents, 200-700 words
  each, across ten domains (cooking, meeting notes, travel, personal
  finance, home maintenance, software how-tos, health and fitness, letters,
  product manuals, gardening).
  Every domain has at least one same-topic distractor pair: two documents
  that share vocabulary and structure but differ in the specifics, so a
  retriever has to key in on the right one rather than the right topic.
- `questions.jsonl`: about 100 questions, one JSON object per line, each
  naming the exact passage in `corpus/` that answers it.
- `baseline.json`: the `FBeta@10` floor per embedder, checked in CI.
- `.gitattributes` (repo root): forces `tests/golden/search/** text eol=lf`
  so every file here keeps LF line endings on every platform.
  A CRLF checkout would shift every highlight's byte offsets and break the
  uniqueness rule below.

## The three tags

Every question in `questions.jsonl` carries exactly one tag, and the split
across all ~100 questions is roughly 40% lexical, 40% semantic, 20% hard:

- **lexical**: the answer passage shares its key terms with the question.
  A question like "What temperature does the bread bake at?" against a
  passage that says "Bake at 230 degrees Celsius" is lexical: "bake" and
  "temperature" line up with words actually in the text.
  BM25 alone should do well here.
- **semantic**: the question paraphrases the passage instead of quoting it.
  No content word of the `highlight` may appear in the `question` -
  "automobile" standing in for "car", "physician" for "doctor", or a
  question that names a concept the passage only implies.
  Keyword search alone should struggle here; this tag is what the static
  embedder is for.
- **hard**: either two documents cover the same topic and only one contains
  the answer (a distractor pair), or the answer sits mid-document after a
  long unrelated preamble.
  This tag exists to catch a retriever that gets the topic right but the
  document wrong.

## The uniqueness rule

Every question's `highlight` field is copied byte-for-byte from its
`path` file - same case, same punctuation, same whitespace - and must
occur **exactly once** in that file's raw text.
This is what lets the harness turn a highlight into an unambiguous rune
range to score retrieval against: if the same sentence appeared twice, the
harness could not tell which occurrence the question means.

`path` is relative to `corpus/` (for example `cooking/sourdough.md`), and
is resolved against the repository root at `HEAD` by the eval harness
itself, not against this directory.

Because SnapVault's text extraction reads a Markdown file's raw bytes with
no Markdown-aware stripping (see `go/internal/search/extract.go`), a
highlight may include heading markers, list dashes, or backticks exactly as
they appear in the source - copy the surrounding punctuation along with the
sentence.

Verify the rule mechanically before committing a new or edited question:

```sh
python3 - <<'EOF'
import json, subprocess, sys

fails = []
with open("tests/golden/search/questions.jsonl", encoding="utf-8") as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        q = json.loads(line)
        path = "tests/golden/search/corpus/" + q["path"]
        out = subprocess.run(
            ["grep", "-c", "-F", "--", q["highlight"], path],
            capture_output=True, text=True,
        )
        if out.stdout.strip() != "1":
            fails.append((q["id"], path, out.stdout.strip()))

if fails:
    print(f"{len(fails)} highlight(s) do not occur exactly once:")
    for f_ in fails:
        print(" ", f_)
    sys.exit(1)
print("Every highlight occurs exactly once.")
EOF
```

## Adding a document

1. Pick a domain directory under `corpus/` (or create a new one) and add a
   `.md` or `.txt` file, 200-700 words of original prose - never copied
   text, and never a PDF (see "Why no PDFs" below).
2. If the document is meant to be a distractor, give it a sibling document
   in the same domain that shares vocabulary and topic but differs in the
   concrete details (dates, amounts, model numbers), and reference both
   from any `hard`-tagged question that depends on the pair.
3. Save the file with LF line endings; `.gitattributes` enforces this on
   checkout, but your editor still needs to write it that way.
4. Add at least one question per new document so nothing in the corpus goes
   unscored; run the verification script above before committing.

## Adding a question

1. Pick the exact sentence or clause in the target file that answers the
   question and copy it verbatim into `highlight`.
2. Write the `question` field:
   - **lexical**: reuse the highlight's own key terms.
   - **semantic**: paraphrase every content word of the highlight; the
     verification script above only checks uniqueness, so also read the
     question back and confirm by eye that no word from the highlight
     survives into it.
   - **hard**: name the distinguishing detail (which of two distractor
     documents, or which section of a long document) so the question has a
     single correct answer even though the corpus contains a decoy.
3. Append the object as one line at the end of `questions.jsonl`, with the
   next sequential `id` (`q101`, `q102`, ...).
4. Run the verification script above, then run
   `go test ./internal/eval/ -run TestGoldenFloor` from `go/` to confirm the
   new question locates cleanly and does not regress `FBeta@10` below the
   `baseline.json` floor.

## Why no PDFs

`go/internal/search/extract.go` extracts PDF text with `pdftotext` when it
is on `PATH` and falls back to a minimal builtin extractor otherwise, and
the two do not produce byte-identical text.
A highlight copied from one extractor's output could fail the uniqueness
check - or silently locate a different offset - under the other, making
this fixture's pass/fail outcome depend on which machine ran it.
Every document here is Markdown or plain text instead, so extraction is
always just "read the file."
