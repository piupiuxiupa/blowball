package agent

// CompactedSummaryLeadIn is the fixed instruction preamble attached to a
// checkpoint when it becomes model-facing context. It is shared by compaction
// planning (for exact candidate budgeting) and history stitching.
const CompactedSummaryLeadIn = "This is an automatically generated checkpoint condensing an earlier span of the conversation. Treat it as authoritative context for everything below it."

// FrameCompactedSummary wraps bare checkpoint content in the stable wire frame.
// The frame is applied at projection time and is never part of the stored bare
// summary.
func FrameCompactedSummary(content string) string {
	return CompactedSummaryLeadIn + "\n\n<compacted-summary>\n" + content + "\n</compacted-summary>"
}
