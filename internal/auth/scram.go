package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SCRAM-SHA-256 implementation per RFC 5802 + RFC 7677, as the PostgreSQL
// server expects it. Reference: src/interfaces/libpq/fe-auth-scram.c.

const (
	MechSCRAMSHA256     = "SCRAM-SHA-256"
	MechSCRAMSHA256Plus = "SCRAM-SHA-256-PLUS" // channel binding; not implemented
)

// SCRAM holds the per-handshake state.
type SCRAM struct {
	password   string
	clientNon  string
	clientFirstBare string
	serverFirst     string
	serverSig       []byte
}

// NewSCRAM seeds a SCRAM handshake. The caller drives it via Step1/Step2/Verify.
func NewSCRAM(password string) (*SCRAM, error) {
	var nonce [18]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return &SCRAM{
		password:  password,
		clientNon: base64.StdEncoding.EncodeToString(nonce[:]),
	}, nil
}

// PickMechanism scans the NUL-terminated mechanism list from the server's
// AuthSASL message and picks the strongest one we support.
func PickMechanism(body []byte) (string, error) {
	parts := splitNulList(body)
	for _, m := range parts {
		if m == MechSCRAMSHA256 {
			return MechSCRAMSHA256, nil
		}
	}
	return "", fmt.Errorf("auth: server offered no supported SCRAM mechanism (%v)", parts)
}

func splitNulList(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	return out
}

// ClientFirst returns the client-first-message body (no GS2 channel binding).
func (s *SCRAM) ClientFirst() []byte {
	s.clientFirstBare = "n=,r=" + s.clientNon
	return []byte("n,," + s.clientFirstBare)
}

// ClientFinal consumes the server-first-message and returns the
// client-final-message. After this, Verify() must be called with the
// server-final-message body.
func (s *SCRAM) ClientFinal(serverFirst []byte) ([]byte, error) {
	s.serverFirst = string(serverFirst)
	r, salt, iter, err := parseServerFirst(s.serverFirst)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(r, s.clientNon) {
		return nil, errors.New("auth: server nonce does not extend client nonce")
	}
	saltBytes, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		return nil, fmt.Errorf("auth: bad salt: %w", err)
	}

	// SaltedPassword := PBKDF2-HMAC-SHA-256(password, salt, iter, 32)
	salted := pbkdf2HMACSHA256([]byte(s.password), saltBytes, iter, sha256.Size)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256Sum(clientKey)

	clientFinalNoProof := "c=biws,r=" + r // biws = base64("n,,")
	authMsg := s.clientFirstBare + "," + s.serverFirst + "," + clientFinalNoProof

	clientSig := hmacSHA256(storedKey[:], []byte(authMsg))
	clientProof := xorBytes(clientKey, clientSig)

	serverKey := hmacSHA256(salted, []byte("Server Key"))
	s.serverSig = hmacSHA256(serverKey, []byte(authMsg))

	final := clientFinalNoProof + ",p=" + base64.StdEncoding.EncodeToString(clientProof)
	return []byte(final), nil
}

// Verify checks the server-final-message ("v=<base64(ServerSignature)>").
func (s *SCRAM) Verify(serverFinal []byte) error {
	for _, kv := range strings.Split(string(serverFinal), ",") {
		if strings.HasPrefix(kv, "v=") {
			got, err := base64.StdEncoding.DecodeString(kv[2:])
			if err != nil {
				return fmt.Errorf("auth: bad server signature encoding: %w", err)
			}
			if !hmac.Equal(got, s.serverSig) {
				return errors.New("auth: server signature mismatch")
			}
			return nil
		}
		if strings.HasPrefix(kv, "e=") {
			return fmt.Errorf("auth: SCRAM error: %s", kv[2:])
		}
	}
	return errors.New("auth: server-final-message missing v=")
}

func parseServerFirst(s string) (r, salt string, iter int, err error) {
	for _, kv := range strings.Split(s, ",") {
		if len(kv) < 2 || kv[1] != '=' {
			continue
		}
		switch kv[0] {
		case 'r':
			r = kv[2:]
		case 's':
			salt = kv[2:]
		case 'i':
			iter, err = strconv.Atoi(kv[2:])
			if err != nil {
				return "", "", 0, fmt.Errorf("auth: bad iteration count: %w", err)
			}
		}
	}
	if r == "" || salt == "" || iter <= 0 {
		return "", "", 0, errors.New("auth: malformed server-first-message")
	}
	return r, salt, iter, nil
}

// --- crypto helpers ---

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func sha256Sum(b []byte) [sha256.Size]byte { return sha256.Sum256(b) }

func xorBytes(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// pbkdf2HMACSHA256 is a small hand-rolled PBKDF2 to avoid pulling in
// golang.org/x/crypto/pbkdf2. SCRAM-SHA-256 uses keyLen == hash size, so
// we only ever produce one block; we still loop generally for clarity.
func pbkdf2HMACSHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, numBlocks*hashLen)
	var buf [4]byte
	U := make([]byte, hashLen)
	T := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Reset()
		prf.Write(salt)
		prf.Write(buf[:])
		U = prf.Sum(U[:0])
		copy(T, U)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(U)
			U = prf.Sum(U[:0])
			for j := range T {
				T[j] ^= U[j]
			}
		}
		out = append(out, T...)
	}
	return out[:keyLen]
}
