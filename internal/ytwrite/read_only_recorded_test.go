package ytwrite

import "testing"

// TestRecordedReadOnly pins the read-only match on the text an op's
// last_error holds (hk 5506). The first row is the string writeFailed
// recorded before hk 5494 (#521) for the eight ops the requeue is for: the
// same RemoteError.Error and APIError.Error shapes as today.
func TestRecordedReadOnly(t *testing.T) {
	const maintenance = "youtrack answered 418: youtrack POST /api/issues/HK-1/comments: 418 invalid_state: The application is currently in read-only mode. Please try again later."
	cases := []struct {
		name     string
		reason   string
		recorded string
		want     bool
	}{
		{"recorded maintenance answer", "http_418", maintenance, true},
		{"article create path", "http_418", "youtrack answered 418: youtrack POST /api/articles: 418 invalid_state: The application is currently in read-only mode.", true},
		{"other 418 code, description quotes the read-only code", "http_418",
			"youtrack answered 418: youtrack POST /api/issues/HK-1/comments: 418 invalid_request: the field invalid_state is unknown", false},
		{"418 with no code", "http_418", "youtrack answered 418: youtrack POST /api/issues/HK-1/comments: http 418", false},
		{"418 code in the description only", "http_418",
			"youtrack answered 418: youtrack POST /api/x: 418 gone: invalid_state: 418 invalid_state", false},
		{"other status with the code", "http_400", "youtrack answered 400: youtrack POST /api/x: 400 invalid_state: bad", false},
		{"reason is not http_418", "http_500", maintenance, false},
		{"status and APIError status disagree", "http_418", "youtrack answered 418: youtrack POST /api/x: 409 invalid_state: x", false},
		{"not an hk-recorded refusal", "http_418", "418 invalid_state: The application is currently in read-only mode.", false},
		{"empty", "http_418", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RecordedReadOnly(c.reason, c.recorded); got != c.want {
				t.Fatalf("RecordedReadOnly(%q, %q) = %v, want %v", c.reason, c.recorded, got, c.want)
			}
		})
	}
}

// TestRecordedReadOnlyMatchesRemoteError ties the parser to the type it
// parses: an error built the way writeFailed records one is matched, and so
// is the same error with another code.
func TestRecordedReadOnlyMatchesRemoteError(t *testing.T) {
	ro := &RemoteError{Status: 418, Code: readOnlyCode, Message: "youtrack POST /api/issues/HK-1/comments: 418 invalid_state: The application is currently in read-only mode."}
	if !RecordedReadOnly("http_418", ro.Error()) {
		t.Fatalf("RemoteError.Error() %q not matched", ro.Error())
	}
	other := &RemoteError{Status: 418, Code: "invalid_request", Message: "youtrack POST /api/x: 418 invalid_request: nope"}
	if RecordedReadOnly("http_418", other.Error()) {
		t.Fatalf("RemoteError.Error() %q matched but its code is not read-only", other.Error())
	}
}
