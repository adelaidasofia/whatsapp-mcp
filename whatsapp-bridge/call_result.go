package main

import "strings"

// The closed vocabulary the calls.result column stores.
//
// Every one of these is written through callResultFromWireReason or the
// callResultOffered constant, never as a literal at a call site. That is the
// same single-declaration discipline rawTypeForStorage uses, and it exists for
// the same reason: the original defect was a writer emitting a value the
// schema did not allow ('offered'), with the schema and the writer edited in
// different files by different changes and nothing tying them together.
const (
	callResultOffered  = "offered"  // an offer arrived; no outcome yet
	callResultAnswered = "answered" //
	callResultMissed   = "missed"   //
	callResultRejected = "rejected" //
	callResultEnded    = "ended"    // completed and hung up
	callResultFailed   = "failed"   // did not complete for a technical reason
	callResultUnknown  = "unknown"  // wire reason not recognized; see result_raw
)

// wireReasonToResult maps the reason string WhatsApp puts on a call terminate
// to the stored vocabulary.
//
// Only unambiguous reasons are mapped. A reason whose meaning has to be guessed
// is deliberately left out so it lands in callResultUnknown with the wire value
// preserved in result_raw — the mapping is then extended from observed data
// rather than from assumptions about a protocol nobody here controls. 'busy' is
// the current example: plainly not a completed call, but whether it belongs
// with rejected or failed is a guess, and a wrong guess is indistinguishable
// from a right one once it is in the column.
var wireReasonToResult = map[string]string{
	"timeout":         callResultMissed,
	"reject":          callResultRejected,
	"rejected":        callResultRejected,
	"decline":         callResultRejected,
	"declined":        callResultRejected,
	"accept":          callResultAnswered,
	"accepted":        callResultAnswered,
	"answered":        callResultAnswered,
	"hangup":          callResultEnded,
	"bye":             callResultEnded,
	"end":             callResultEnded,
	"ended":           callResultEnded,
	"connection-lost": callResultFailed,
	"connection_lost": callResultFailed,
	"failed":          callResultFailed,
	"error":           callResultFailed,
}

// callResultFromWireReason converts a raw wire reason into (result, result_raw).
//
// The second return is bound straight into SQL, so it is `any`: the verbatim
// wire string when there was one, nil (SQL NULL) when the reason was empty.
// Keeping the raw value even for reasons we DO recognize is what makes the map
// above auditable against production data instead of only against this file.
//
// An empty reason yields callResultEnded, preserving the behavior the original
// handler had for that case — a terminate with no stated reason is the normal
// end of a normal call.
func callResultFromWireReason(reason string) (string, any) {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return callResultEnded, nil
	}
	// The wire chooses this string's case; we do not trust it to be stable.
	if mapped, ok := wireReasonToResult[strings.ToLower(trimmed)]; ok {
		return mapped, reason
	}
	return callResultUnknown, reason
}
