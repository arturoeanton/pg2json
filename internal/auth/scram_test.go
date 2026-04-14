package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// Vector from RFC 7677 §3 (SCRAM-SHA-256 example).
func TestSCRAM_RFC7677Vector(t *testing.T) {
	// password: pencil, salt: W22ZaJ0SNY7soEsUEjb6gQ==, iter: 4096
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	salted := pbkdf2HMACSHA256([]byte("pencil"), salt, 4096, 32)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)

	clientFirstBare := "n=user,r=rOprNGfwEbeRWgbNEkqO"
	serverFirst := "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	clientFinalNoProof := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	authMsg := clientFirstBare + "," + serverFirst + "," + clientFinalNoProof

	clientSig := hmacSHA256(storedKey[:], []byte(authMsg))
	clientProof := xorBytes(clientKey, clientSig)
	got := base64.StdEncoding.EncodeToString(clientProof)
	want := "dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if got != want {
		t.Fatalf("ClientProof mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestSCRAM_ClientFlow(t *testing.T) {
	s, err := NewSCRAM("pencil")
	if err != nil {
		t.Fatal(err)
	}
	first := s.ClientFirst()
	if !strings.HasPrefix(string(first), "n,,n=,r=") {
		t.Fatalf("bad client-first: %s", first)
	}
}

func TestPickMechanism(t *testing.T) {
	body := []byte("SCRAM-SHA-256\x00\x00")
	m, err := PickMechanism(body)
	if err != nil || m != MechSCRAMSHA256 {
		t.Fatalf("got %s err %v", m, err)
	}
	body = []byte("OTHER\x00\x00")
	if _, err := PickMechanism(body); err == nil {
		t.Fatal("expected error")
	}
}

func TestHMACInterop(t *testing.T) {
	// Sanity check: my hmacSHA256 must equal stdlib usage exactly.
	a := hmacSHA256([]byte("k"), []byte("v"))
	m := hmac.New(sha256.New, []byte("k"))
	m.Write([]byte("v"))
	b := m.Sum(nil)
	if !hmac.Equal(a, b) {
		t.Fatal("hmac differs")
	}
}
