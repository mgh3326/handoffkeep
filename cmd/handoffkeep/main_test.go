package main

import "testing"

func TestListenValidation(t *testing.T) {
	for _, a := range []string{"0.0.0.0:8800", "8.8.8.8:8800", "[::]:8800"} {
		if validListen(a, false) == nil {
			t.Fatalf("unsafe bind accepted: %s", a)
		}
	}
	if err := validListen("127.0.0.1:8800", false); err != nil {
		t.Fatal(err)
	}
	if err := validListen("100.122.100.56:8800", true); err != nil {
		t.Fatal(err)
	}
}
