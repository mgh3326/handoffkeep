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
