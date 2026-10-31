package lab

import "testing"

func TestDeclareKinds(t *testing.T) {
	l := &Lab{}
	if k := l.Kinds(); k == nil || len(k) != 0 {
		t.Fatalf("Kinds() = %#v, want an empty non-nil list (a lab that declares nothing is not one that is unknown)", k)
	}
	l.DeclareKinds("settle", "event.*", "chat.out")
	l.DeclareKinds("chat.out", "clock.step")
	got := l.Kinds()
	want := []string{"chat.out", "clock.step", "event.*", "settle"}
	if len(got) != len(want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Kinds() = %v, want %v (sorted, no duplicates)", got, want)
		}
	}
}
