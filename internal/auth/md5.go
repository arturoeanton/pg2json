// Package auth implements the small bits of PostgreSQL's authentication
// flow that we support: cleartext password and MD5. SCRAM is intentionally
// stubbed out; see scram_todo.go.
package auth

import (
	"crypto/md5"
	"encoding/hex"
)

// MD5Password returns the "md5"+hex(md5(hex(md5(password+user))+salt)) form
// expected by the server for AuthRequestMD5. See the comment in
// src/backend/libpq/crypt.c for the canonical recipe.
func MD5Password(user, password string, salt []byte) string {
	inner := md5.Sum([]byte(password + user))
	innerHex := hex.EncodeToString(inner[:])

	outer := md5.New()
	outer.Write([]byte(innerHex))
	outer.Write(salt)
	outerHex := hex.EncodeToString(outer.Sum(nil))

	return "md5" + outerHex
}
