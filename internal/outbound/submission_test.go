package outbound

import "testing"

func TestReturnPathUsesSenderDomain(t *testing.T) {
	got := ReturnPath("01ARZ3NDEKTSV4RRFFQ69G5FAV", "emails.ax")
	want := "bounces+01ARZ3NDEKTSV4RRFFQ69G5FAV@emails.ax"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
