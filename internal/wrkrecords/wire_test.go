package wrkrecords

import (
	"bytes"
	"os"
	"testing"
)

func TestStrictJSONAndCanonicalOrder(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"a":1,"a":2}`), []byte(`{"x":"\ud800"}`), []byte("{} {}"), {0xff}} {
		if _, err := Decode(raw); err == nil {
			t.Fatal("non-strict JSON accepted")
		}
	}
	v, err := Decode([]byte(`{"z":1e-7,"a":[-0,0.000001,1e21],"😀":1,"�":2}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"a":[0,0.000001,1e+21],"z":1e-7,"😀":1,"�":2}` {
		t.Fatalf("canonical JSON: %s", b)
	}
}
func TestContractCopyPinned(t *testing.T) {
	b, err := os.ReadFile("../../testdata/o3/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, []byte(contractSchema)) {
		t.Fatal("contract differs from fixed pack")
	}
}
