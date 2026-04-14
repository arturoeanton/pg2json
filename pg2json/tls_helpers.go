package pg2json

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
)

var errNoTLSChain = errors.New("pg2json: server returned no TLS chain")

func tlsVerifyOptionsNoHost(cs tls.ConnectionState) x509.VerifyOptions {
	intermediates := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		intermediates.AddCert(c)
	}
	return x509.VerifyOptions{
		Intermediates: intermediates,
		// Roots: nil -> system roots
	}
}
