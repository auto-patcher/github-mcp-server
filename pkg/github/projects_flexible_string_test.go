package github

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlexibleString_UnmarshalJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		want    FlexibleString
		wantErr bool
	}{
		{"plain string", `"Done"`, "Done", false},
		{"object with name", `{"name":"Done","id":"x"}`, "Done", false},
		{"object empty name", `{"name":""}`, "", false},
		{"invalid", `123`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var f FlexibleString
			err := json.Unmarshal([]byte(tc.input), &f)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, f)
			}
		})
	}
}
