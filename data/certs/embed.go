package certs

import "embed"

//go:embed ca.cert.pem
//go:embed localhost.cert.pem
//go:embed localhost.key.pem
var CertFS embed.FS
