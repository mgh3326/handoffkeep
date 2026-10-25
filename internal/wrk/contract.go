package wrk

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// ContractVersion is the wire contract string of design §5.
const ContractVersion = "hk1873.v1"

// contractJSON is a byte-exact copy of testdata/o3/contract.json (the M1
// pack's copy of the design §5 bundle); a test pins the two together.
//
//go:embed contract.json
var contractJSON []byte

// ContractJSON returns the embedded §5 bundle.
func ContractJSON() []byte { return append([]byte(nil), contractJSON...) }

var (
	contractOnce sync.Once
	contractDefs map[string]*jsonschema.Schema
	contractErr  error
	resolvedMu   sync.Mutex
	resolved     = map[string]*jsonschema.Resolved{}
)

func loadContract() {
	var root jsonschema.Schema
	if contractErr = json.Unmarshal(contractJSON, &root); contractErr != nil {
		return
	}
	contractDefs = root.Defs
}

// ValidateDef checks v (DecodeStrict shape) against one $defs entry of the
// bundle. The closed object schemas reject unknown and missing fields.
func ValidateDef(def string, v any) error {
	contractOnce.Do(loadContract)
	if contractErr != nil {
		return contractErr
	}
	resolvedMu.Lock()
	rs, ok := resolved[def]
	if !ok {
		if _, exists := contractDefs[def]; !exists {
			resolvedMu.Unlock()
			return fmt.Errorf("contract has no $defs.%s", def)
		}
		wrapper := &jsonschema.Schema{Schema: "https://json-schema.org/draft/2020-12/schema", Ref: "#/$defs/" + def, Defs: contractDefs}
		var err error
		if rs, err = wrapper.Resolve(nil); err != nil {
			resolvedMu.Unlock()
			return err
		}
		resolved[def] = rs
	}
	resolvedMu.Unlock()
	return rs.Validate(v)
}
