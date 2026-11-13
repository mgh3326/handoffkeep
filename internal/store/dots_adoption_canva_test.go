package store

import "testing"

const dotsCanvaURL = "https://www.canva.com/design/DAF123/edit"

func TestCheckDotsAdoptionCanva(t *testing.T) {
	canva := func(in DotsAdoptionInput) DotsAdoptionInput {
		in.CanvaURL, in.CanvaPage, in.CanvaVersion = dotsCanvaURL, "3", "v4"
		return in
	}
	cases := []struct {
		name string
		in   DotsAdoptionInput
		ok   bool
	}{
		{"adoption without a Canva source is unchanged", DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12"}, true},
		{"accepted names its Canva page and version", canva(DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12"}), true},
		{"partially_used names its Canva page and version", canva(DotsAdoptionInput{State: DotsAdoptionPartial, Ref: "hk:task/12"}), true},
		{"rejected cannot name an adopted design", canva(DotsAdoptionInput{State: DotsAdoptionRejected}), false},
		{"superseded cannot name an adopted design", canva(DotsAdoptionInput{State: DotsAdoptionSuperseded, SupersededBy: 7}), false},
		{"a non-https Canva URL is refused", func() DotsAdoptionInput {
			in := canva(DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12"})
			in.CanvaURL = "http://www.canva.com/design/DAF123/edit"
			return in
		}(), false},
		{"a Canva page without a version is refused", func() DotsAdoptionInput {
			in := canva(DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12"})
			in.CanvaVersion = ""
			return in
		}(), false},
		{"a Canva URL without a page is refused", func() DotsAdoptionInput {
			in := canva(DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12"})
			in.CanvaPage = ""
			return in
		}(), false},
		{"a Canva link outside a design is refused", func() DotsAdoptionInput {
			in := canva(DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12"})
			in.CanvaURL = "https://www.canva.com/folder/DAF123"
			return in
		}(), false},
		{"a link to another host is refused", func() DotsAdoptionInput {
			in := canva(DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12"})
			in.CanvaURL = "https://example.com/design/DAF123/edit"
			return in
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDotsAdoption(1, tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok = %v", err, tc.ok)
			}
		})
	}
}

func TestKeepDotsAdoptionCanva(t *testing.T) {
	recorded := DotsResult{AdoptionState: DotsAdoptionAccepted, CanvaURL: dotsCanvaURL, CanvaPage: "3", CanvaVersion: "v4"}
	adopt := DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12"}
	recordedAdopt := DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12", CanvaURL: dotsCanvaURL, CanvaPage: "3", CanvaVersion: "v4"}
	cases := []struct {
		name string
		cur  DotsResult
		in   DotsAdoptionInput
		want DotsAdoptionInput
	}{
		{"a repeat with no Canva fields keeps the recorded design", recorded, adopt, recordedAdopt},
		{"partially_used with no Canva fields keeps the recorded design", recorded, DotsAdoptionInput{State: DotsAdoptionPartial, Ref: "hk:task/12"}, DotsAdoptionInput{State: DotsAdoptionPartial, Ref: "hk:task/12", CanvaURL: dotsCanvaURL, CanvaPage: "3", CanvaVersion: "v4"}},
		{"a named design replaces the recorded one", recorded, DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12", CanvaURL: "https://www.canva.com/design/DAF999/edit", CanvaPage: "1", CanvaVersion: "v5"}, DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: "hk:task/12", CanvaURL: "https://www.canva.com/design/DAF999/edit", CanvaPage: "1", CanvaVersion: "v5"}},
		{"rejected clears the recorded design", recorded, DotsAdoptionInput{State: DotsAdoptionRejected}, DotsAdoptionInput{State: DotsAdoptionRejected}},
		{"nothing recorded stays empty", DotsResult{}, adopt, adopt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepDotsAdoptionCanva(tc.cur, tc.in); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
