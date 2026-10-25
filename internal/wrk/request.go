package wrk

// Request is one parsed POST /v1/wrk/jobs/{job}/records body. Body is the
// complete parsed object the request digest covers; Content holds the exact
// decoded evidence bytes (content_b64 or report_b64) when the kind has them.
type Request struct {
	JobID       string
	Kind        string
	ClaimSeq    int64
	ExpectedSeq int64
	Payload     map[string]any
	Body        map[string]any
	Content     []byte
	SHA256      string
}

// ParseRequest strictly decodes and validates a request body against the
// closed $defs.Request union plus the formats JSON Schema leaves as
// annotations (date-time, uri, canonical base64). jobID is the already
// validated path job id.
func ParseRequest(jobID string, raw []byte) (*Request, error) {
	v, err := DecodeStrict(raw)
	if err != nil {
		return nil, Fail(InvalidRequest)
	}
	body, ok := v.(map[string]any)
	if !ok {
		return nil, Fail(InvalidRequest)
	}
	if err := ValidateDef("Request", body); err != nil {
		return nil, Fail(InvalidRequest)
	}
	r := &Request{JobID: jobID, Body: body, Kind: body["kind"].(string), Payload: body["payload"].(map[string]any)}
	r.ClaimSeq, _ = pInt(body, "claim_seq")
	r.ExpectedSeq, _ = pInt(body, "expected_seq")
	p := r.Payload
	for _, k := range []string{"expires_at", "original_at"} {
		if s, ok := p[k].(string); ok {
			if _, ok := parseDateTime(s); !ok {
				return nil, Fail(InvalidRequest)
			}
		}
	}
	contentKey := ""
	switch r.Kind {
	case "verdict", "import":
		contentKey = "content_b64"
	case "joined", "completed":
		contentKey = "report_b64"
	}
	if contentKey != "" {
		b, tooLarge, ok := DecodeContent(p[contentKey].(string))
		if tooLarge {
			return nil, Fail(RequestTooLarge)
		}
		if !ok {
			return nil, Fail(InvalidRequest)
		}
		r.Content = b
	}
	if r.SHA256, err = RequestDigest(jobID, body); err != nil {
		return nil, Fail(InvalidRequest)
	}
	return r, nil
}

// ParseInput validates a CLI stdin payload for kind against Input_<kind>.
func ParseInput(kind string, raw []byte) (map[string]any, error) {
	v, err := DecodeStrict(raw)
	if err != nil {
		return nil, Fail(InvalidRequest)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, Fail(InvalidRequest)
	}
	known := false
	for _, k := range Kinds {
		known = known || k == kind
	}
	if !known {
		return nil, Fail(InvalidRequest)
	}
	if err := ValidateDef("Input_"+kind, obj); err != nil {
		return nil, Fail(InvalidRequest)
	}
	return obj, nil
}
