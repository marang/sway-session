package titleindicator

import (
	"encoding/json"
	"os"
	"testing"
)

// This fixture is shared with the other repository. Changes to v1 must preserve
// these wire bytes and decoding behavior across independently released binaries.
func TestV1WireContract(t *testing.T) {
	data, err := os.ReadFile("testdata/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string
		Container int64
		Marks     []string
		State     State
		Valid     bool
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			state, valid := FromMarks(tc.Marks, tc.Container)
			if state != tc.State || valid != tc.Valid {
				t.Fatalf("decoded (%q, %v), want (%q, %v)", state, valid, tc.State, tc.Valid)
			}
			if tc.Valid {
				encoded, err := Mark(tc.State, tc.Container)
				if err != nil || encoded != tc.Marks[0] {
					t.Fatalf("encoded %q, %v; want %q", encoded, err, tc.Marks[0])
				}
			}
		})
	}
}
