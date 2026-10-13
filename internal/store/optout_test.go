package store

import (
	"errors"
	"testing"
	"time"
)

func TestOptOuts(t *testing.T) {
	s := openStore(t)
	if ok, err := s.OptedOut(maria); ok || err != nil {
		t.Fatal(ok, err)
	}
	if added, err := s.AddOptOut(OptOut{Chat: maria, Reason: "STOP", At: time.Now(), By: "whatsapp"}); !added || err != nil {
		t.Fatal(added, err)
	}
	if added, _ := s.AddOptOut(OptOut{Chat: maria, Reason: "manual", At: time.Now(), By: "ops"}); added {
		t.Fatal("a second entry for the same chat must not replace the first")
	}
	if _, err := s.AddOptOut(OptOut{Chat: john, Reason: "manual", At: time.Now().Add(time.Second), By: "ops"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.OptOuts()
	if err != nil || len(list) != 2 || list[0].Chat != john || list[1].Reason != "STOP" || list[1].By != "whatsapp" {
		t.Fatalf("%v %+v", err, list)
	}
	if ok, _ := s.OptedOut(maria); !ok {
		t.Fatal("maria opted out")
	}
	if err := s.RemoveOptOut(maria); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveOptOut(maria); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove = %v", err)
	}
	if ok, _ := s.OptedOut(maria); ok {
		t.Fatal("removed")
	}
}

func TestFold(t *testing.T) {
	for in, want := range map[string]string{"STOP": "stop", "Não": "nao", "DESCADASTRAR": "descadastrar", "Pará": "para"} {
		if got := Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
}
