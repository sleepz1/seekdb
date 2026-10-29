# Full-text parser performance tuning

This branch keeps the existing tokenization and stopword semantics while
removing repeated setup from the document hot path.

## Changes

- `ObFTParseHelper` retains one parser iterator for the lifetime of a helper.
  BENG reopens its existing tokenizer/normalizer pipeline and IK rebinds its
  existing tokenize context. IK dictionaries and segmenter objects are no
  longer rebuilt for every document.
- IK's `TokenizeContext::reuse_document` clears result state and prepares the
  next document without reallocating its list containers.
- Stopword conversion uses a request-local reusable scratch arena. When the
  word and stopword collations match, conversion is skipped entirely.

The cache is local to a parser helper, so parser instances remain isolated
between concurrent helpers. A parser that does not advertise reuse keeps the
original create-and-free behavior.

## Validation

`git diff --check` passes. The repository checkout used for this change is a
sparse source checkout and does not contain the complete seekdb build
dependencies, so a full server build and the contest benchmark must be run in
the official Linux build environment before comparing scores.
