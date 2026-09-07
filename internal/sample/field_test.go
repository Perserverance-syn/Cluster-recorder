package sample

import (
	"encoding/json"
	"testing"
)

func TestValidate(t *testing.T) {
	bad := []Field{{State: StatePresent}, {State: StateAbsent, Value: 1}, {State: "gone"}}
	for _, f := range bad {
		if f.Validate() == nil {
			t.Errorf("%+v should be invalid", f)
		}
	}
	good := []Field{Present(0), Absent(), NotApplicable("x"), Unknown("y")}
	for _, f := range good {
		if err := f.Validate(); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
}

func TestEqualSurvivesJSONRoundTrip(t *testing.T) {
	orig := Present(1500)
	b, _ := json.Marshal(orig)
	var back Field
	_ = json.Unmarshal(b, &back) // Value is now float64(1500)
	if !orig.Equal(back) {
		t.Fatal("int and float64 of same number must compare equal")
	}
	if Present("a").Equal(Present("b")) || Absent().Equal(Present("a")) {
		t.Fatal("distinct fields compared equal")
	}
	if !Unknown("a").Equal(Unknown("b")) {
		t.Fatal("reason must not affect equality")
	}
}
