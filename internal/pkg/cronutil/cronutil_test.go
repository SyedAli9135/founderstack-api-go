package cronutil

import "testing"

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		expr    string
		wantErr bool
	}{
		{"* * * * *", true},   // every minute
		{"*/2 * * * *", true}, // every 2 minutes
		{"*/4 * * * *", true},
		{"0,1 * * * *", true},  // a 1-minute gap hiding in the hour
		{"*/1 9 * * *", true},  // quiet all day, every minute during 9am
		{"*/5 * * * *", false}, // exactly the minimum
		{"*/15 * * * *", false},
		{"0 9 * * 1-5", false}, // weekdays at 9
		{"0 0 1 * *", false},   // monthly
		{"30 2 29 2 *", false}, // only on a leap day
		{"not a cron", true},   // malformed
		{"", true},
	} {
		_, err := Validate(tc.expr)
		if (err != nil) != tc.wantErr {
			t.Errorf("Validate(%q) err = %v, wantErr %v", tc.expr, err, tc.wantErr)
		}
	}
}
