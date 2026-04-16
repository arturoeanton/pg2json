package types

import (
	"encoding/binary"
	"testing"
)

// buildNumeric assembles a synthetic numeric wire body for the given
// digits/weight/sign/dscale. digits are base-10000 values in [0, 9999].
func buildNumeric(digits []uint16, weight int16, sign uint16, dscale uint16) []byte {
	buf := make([]byte, 8+2*len(digits))
	binary.BigEndian.PutUint16(buf[0:2], uint16(int16(len(digits))))
	binary.BigEndian.PutUint16(buf[2:4], uint16(weight))
	binary.BigEndian.PutUint16(buf[4:6], sign)
	binary.BigEndian.PutUint16(buf[6:8], dscale)
	for i, d := range digits {
		binary.BigEndian.PutUint16(buf[8+2*i:], d)
	}
	return buf
}

func TestEncodeNumericBinary(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"zero_no_scale", buildNumeric(nil, 0, 0x0000, 0), "0"},
		{"zero_with_scale", buildNumeric(nil, 0, 0x0000, 3), "0.000"},
		{"one_two_three_dot_four_five",
			buildNumeric([]uint16{123, 4500}, 0, 0x0000, 2), "123.45"},
		{"neg_one_two_three_dot_four_five",
			buildNumeric([]uint16{123, 4500}, 0, 0x4000, 2), "-123.45"},
		{"one_thousand_dot_zero_zero",
			buildNumeric([]uint16{1000}, 0, 0x0000, 2), "1000.00"},
		{"ten_thousand_dot_zero_zero",
			buildNumeric([]uint16{1}, 1, 0x0000, 2), "10000.00"},
		{"small_frac_0.001",
			buildNumeric([]uint16{10}, -1, 0x0000, 3), "0.001"},
		{"tiny_frac_0.00001",
			buildNumeric([]uint16{1000}, -2, 0x0000, 5), "0.00001"},
		{"big_integer",
			buildNumeric([]uint16{42, 0}, 1, 0x0000, 0), "420000"},
		{"leading_non_padded",
			buildNumeric([]uint16{7, 42}, 1, 0x0000, 0), "70042"},
		{"nan", buildNumeric(nil, 0, 0xC000, 0), `"NaN"`},
		{"pos_inf", buildNumeric(nil, 0, 0xD000, 0), `"Infinity"`},
		{"neg_inf", buildNumeric(nil, 0, 0xF000, 0), `"-Infinity"`},
		{"null", nil, "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(encodeNumericBinary(nil, tc.raw))
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
