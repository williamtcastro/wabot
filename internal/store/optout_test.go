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

func TestOptOutUnderLIDIsRelinked(t *testing.T) {
	s := openStore(t)
	if _, err := s.db.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	const mariaLID, johnLID = "100000000000001@lid", "100000000000002@lid"
	for _, chat := range []string{mariaLID, johnLID, john} {
		if _, err := s.AddOptOut(OptOut{Chat: chat, Reason: "STOP", At: time.Now(), By: "whatsapp"}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := s.OptedOut(maria, mariaLID); !ok {
		t.Fatal("either key must match")
	}
	if _, err := s.db.Exec(`INSERT INTO whatsmeow_lid_map VALUES ('100000000000001', '5511999999999'), ('100000000000002', '5511988887777')`); err != nil {
		t.Fatal(err)
	}
	list, err := s.OptOuts()
	if err != nil || len(list) != 2 {
		t.Fatalf("one entry per person once the numbers are known: %v %+v", err, list)
	}
	if ok, _ := s.OptedOut(maria); !ok {
		t.Fatal("the @lid entry must move to the phone JID")
	}
	if err := s.RemoveOptOut(maria, mariaLID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveOptOut(maria, mariaLID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove = %v", err)
	}
	if err := s.RemoveOptOut(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no keys = %v", err)
	}
}

func TestFold(t *testing.T) {
	for in, want := range map[string]string{"STOP": "stop", "Não": "nao", "DESCADASTRAR": "descadastrar", "Pará": "para"} {
		if got := Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
}
