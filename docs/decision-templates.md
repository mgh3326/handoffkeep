# Decision templates

Decision templates turn a producer's structured question into Korean display
text without changing the stored request, its options, or its audit history.
They run on read, so open requests recorded before a template was added also
use it. Template translations take precedence over translation-job output;
the original remains available in the console.

To add a template, implement a renderer in a separate file in `internal/store`
and register it in `decisionTemplates`. Match the producer, question prefix,
option contract, and required payload keys. Decode the payload strictly and
return no match if its shape is unknown. Preserve unknown evidence verbatim.
Only derive a recommendation when the evidence justifies the rule; preserve a
producer recommendation and label its origin. Keep the source request and
options unchanged. Add tests using a real sample, malformed payloads, rule
boundaries, and producer recommendations. Check every display surface when the
presentation contract changes.
