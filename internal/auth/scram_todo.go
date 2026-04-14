package auth

// SCRAM-SHA-256 is not yet implemented. Outline for Phase 2:
//
//   1. Server sends Authentication(SASL) with a NUL-terminated list of
//      mechanisms; pick "SCRAM-SHA-256".
//   2. Client sends SASLInitialResponse with the chosen mechanism name and
//      the client-first-message ("n,,n=,r=<nonce>").
//   3. Server replies with Authentication(SASLContinue) carrying the
//      server-first-message; client computes ClientProof and sends
//      SASLResponse with client-final-message.
//   4. Server replies with Authentication(SASLFinal) carrying the server
//      signature; client verifies it, then waits for AuthOK.
//
// All the crypto we need is in crypto/sha256, crypto/hmac, and
// golang.org/x/crypto/pbkdf2 (or a 30-line hand-rolled PBKDF2 to avoid
// the dependency). Reference implementation: src/interfaces/libpq/fe-auth-scram.c.
